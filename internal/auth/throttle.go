package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/authlockout"
)

const storeTimeout = 2 * time.Second

type FailureThrottle struct {
	mu          sync.Mutex
	entries     map[string]*throttleEntry
	maxFailures int
	window      time.Duration
	lockout     time.Duration
	client      *ent.Client
}

type throttleEntry struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

func NewFailureThrottle(maxFailures int, window time.Duration, lockout time.Duration) *FailureThrottle {
	return &FailureThrottle{
		entries:     map[string]*throttleEntry{},
		maxFailures: maxFailures,
		window:      window,
		lockout:     lockout,
	}
}

func (t *FailureThrottle) WithStore(client *ent.Client) *FailureThrottle {
	t.client = client
	return t
}

// Blocked reports whether key is currently locked out. When a shared store is
// configured it is authoritative; a store error other than a missing row is
// treated as blocked so that DB outages cannot bypass the lockout.
func (t *FailureThrottle) Blocked(key string) bool {
	if t.blockedLocally(key) {
		return true
	}
	if t.client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	row, err := t.client.AuthLockout.Get(ctx, key)
	if err != nil {
		if ent.IsNotFound(err) {
			return false
		}
		slog.Warn("auth lockout lookup failed; treating as blocked", "error", err)
		return true
	}
	if row.LockedUntil == nil || !time.Now().Before(*row.LockedUntil) {
		return false
	}
	t.setEntry(key, row)
	return true
}

func (t *FailureThrottle) blockedLocally(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[key]
	if !ok {
		return false
	}
	return time.Now().Before(entry.lockedUntil)
}

func (t *FailureThrottle) setEntry(key string, row *ent.AuthLockout) {
	entry := &throttleEntry{failures: row.Failures, windowStart: row.WindowStart}
	if row.LockedUntil != nil {
		entry.lockedUntil = *row.LockedUntil
	}
	t.mu.Lock()
	t.entries[key] = entry
	t.mu.Unlock()
}

func (t *FailureThrottle) RecordFailure(key string) {
	now := time.Now()
	t.mu.Lock()
	t.pruneExpired(now)
	entry, ok := t.entries[key]
	if !ok || now.Sub(entry.windowStart) > t.window {
		entry = &throttleEntry{windowStart: now}
		t.entries[key] = entry
	}
	entry.failures++
	if entry.failures >= t.maxFailures {
		entry.lockedUntil = now.Add(t.lockout)
	}
	t.mu.Unlock()
	if t.client == nil {
		return
	}
	row, err := t.recordFailureInStore(key, now)
	if err != nil {
		slog.Warn("persist auth lockout failed", "error", err)
		return
	}
	t.setEntry(key, row)
}

// recordFailureInStore atomically increments the shared failure counter,
// resetting it first if its window has expired and no lockout is active, and
// sets locked_until once the shared count reaches maxFailures.
func (t *FailureThrottle) recordFailureInStore(key string, now time.Time) (*ent.AuthLockout, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	lockouts := t.client.AuthLockout
	_, err := lockouts.Update().
		Where(
			authlockout.IDEQ(key),
			authlockout.WindowStartLT(now.Add(-t.window)),
			authlockout.Or(authlockout.LockedUntilIsNil(), authlockout.LockedUntilLT(now)),
		).
		SetFailures(0).
		SetWindowStart(now).
		ClearLockedUntil().
		Save(ctx)
	if err != nil {
		return nil, err
	}
	err = lockouts.Create().
		SetID(key).
		SetFailures(1).
		SetWindowStart(now).
		OnConflictColumns(authlockout.FieldID).
		Update(func(u *ent.AuthLockoutUpsert) { u.AddFailures(1) }).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	row, err := lockouts.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if row.Failures >= t.maxFailures && (row.LockedUntil == nil || !now.Before(*row.LockedUntil)) {
		row, err = lockouts.UpdateOneID(key).SetLockedUntil(now.Add(t.lockout)).Save(ctx)
		if err != nil {
			return nil, err
		}
	}
	return row, nil
}

func (t *FailureThrottle) RecordSuccess(key string) {
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
	if t.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if _, err := t.client.AuthLockout.Delete().Where(authlockout.IDEQ(key)).Exec(ctx); err != nil {
		slog.Warn("clear auth lockout failed", "error", err)
	}
}

func (t *FailureThrottle) pruneExpired(now time.Time) {
	if len(t.entries) < 1024 {
		return
	}
	for key, entry := range t.entries {
		if now.Sub(entry.windowStart) > t.window && now.After(entry.lockedUntil) {
			delete(t.entries, key)
		}
	}
}

func (t *FailureThrottle) PruneStore(ctx context.Context) error {
	if t.client == nil {
		return nil
	}
	cutoff := time.Now().Add(-t.window - t.lockout)
	_, err := t.client.AuthLockout.Delete().Where(authlockout.WindowStartLT(cutoff)).Exec(ctx)
	return err
}

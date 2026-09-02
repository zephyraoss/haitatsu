package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/authlockout"
)

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

func (t *FailureThrottle) Blocked(key string) bool {
	if t.blockedLocally(key) {
		return true
	}
	if t.client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := t.client.AuthLockout.Get(ctx, key)
	if err != nil {
		return false
	}
	if row.LockedUntil == nil || !time.Now().Before(*row.LockedUntil) {
		return false
	}
	t.mu.Lock()
	t.entries[key] = &throttleEntry{failures: row.Failures, windowStart: row.WindowStart, lockedUntil: *row.LockedUntil}
	t.mu.Unlock()
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
	snapshot := *entry
	t.mu.Unlock()
	t.persist(key, snapshot)
}

func (t *FailureThrottle) RecordSuccess(key string) {
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
	if t.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = t.client.AuthLockout.Delete().Where(authlockout.IDEQ(key)).Exec(ctx)
}

func (t *FailureThrottle) persist(key string, entry throttleEntry) {
	if t.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	create := t.client.AuthLockout.Create().SetID(key).SetFailures(entry.failures).SetWindowStart(entry.windowStart)
	if !entry.lockedUntil.IsZero() {
		create.SetLockedUntil(entry.lockedUntil)
	}
	err := create.OnConflictColumns(authlockout.FieldID).UpdateNewValues().Exec(ctx)
	if err != nil {
		slog.Warn("persist auth lockout failed", "error", err)
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

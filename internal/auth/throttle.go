package auth

import (
	"sync"
	"time"
)

type FailureThrottle struct {
	mu          sync.Mutex
	entries     map[string]*throttleEntry
	maxFailures int
	window      time.Duration
	lockout     time.Duration
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

func (t *FailureThrottle) Blocked(key string) bool {
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
	defer t.mu.Unlock()
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
}

func (t *FailureThrottle) RecordSuccess(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
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

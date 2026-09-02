package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	buckets  map[string]*bucket
	lastScan time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(perSecond float64, burst int) *Limiter {
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{rate: perSecond, burst: float64(burst), buckets: map[string]*bucket{}, lastScan: time.Now()}
}

func (l *Limiter) Allow(key string) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastScan) < time.Minute || len(l.buckets) < 4096 {
		return
	}
	l.lastScan = now
	for key, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, key)
		}
	}
}

type ConcurrencyGate struct {
	mu     sync.Mutex
	limit  int
	active map[string]int
}

func NewConcurrencyGate(limit int) *ConcurrencyGate {
	return &ConcurrencyGate{limit: limit, active: map[string]int{}}
}

func (g *ConcurrencyGate) Acquire(key string) bool {
	if g == nil || g.limit <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[key] >= g.limit {
		return false
	}
	g.active[key]++
	return true
}

func (g *ConcurrencyGate) Release(key string) {
	if g == nil || g.limit <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[key] <= 1 {
		delete(g.active, key)
		return
	}
	g.active[key]--
}

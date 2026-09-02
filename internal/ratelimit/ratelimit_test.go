package ratelimit

import "testing"

func TestLimiterBurstThenRefuses(t *testing.T) {
	limiter := New(0.0001, 2)
	if !limiter.Allow("a") || !limiter.Allow("a") {
		t.Fatal("burst should allow two")
	}
	if limiter.Allow("a") {
		t.Fatal("third request should be refused")
	}
	if !limiter.Allow("b") {
		t.Fatal("other keys are independent")
	}
}

func TestConcurrencyGate(t *testing.T) {
	gate := NewConcurrencyGate(1)
	if !gate.Acquire("ip") || gate.Acquire("ip") {
		t.Fatal("gate should allow one")
	}
	gate.Release("ip")
	if !gate.Acquire("ip") {
		t.Fatal("release should free the slot")
	}
}

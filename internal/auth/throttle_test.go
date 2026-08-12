package auth

import (
	"testing"
	"time"
)

func TestFailureThrottleLocksAfterMaxFailures(t *testing.T) {
	throttle := NewFailureThrottle(3, time.Minute, time.Minute)
	if throttle.Blocked("1.2.3.4") {
		t.Fatal("fresh key should not be blocked")
	}
	throttle.RecordFailure("1.2.3.4")
	throttle.RecordFailure("1.2.3.4")
	if throttle.Blocked("1.2.3.4") {
		t.Fatal("blocked before reaching max failures")
	}
	throttle.RecordFailure("1.2.3.4")
	if !throttle.Blocked("1.2.3.4") {
		t.Fatal("not blocked after max failures")
	}
	if throttle.Blocked("5.6.7.8") {
		t.Fatal("other keys should be unaffected")
	}
}

func TestFailureThrottleSuccessClears(t *testing.T) {
	throttle := NewFailureThrottle(2, time.Minute, time.Minute)
	throttle.RecordFailure("1.2.3.4")
	throttle.RecordSuccess("1.2.3.4")
	throttle.RecordFailure("1.2.3.4")
	if throttle.Blocked("1.2.3.4") {
		t.Fatal("success should reset the failure count")
	}
}

func TestFailureThrottleLockoutExpires(t *testing.T) {
	throttle := NewFailureThrottle(1, time.Minute, 10*time.Millisecond)
	throttle.RecordFailure("1.2.3.4")
	if !throttle.Blocked("1.2.3.4") {
		t.Fatal("should be blocked immediately after lockout")
	}
	time.Sleep(20 * time.Millisecond)
	if throttle.Blocked("1.2.3.4") {
		t.Fatal("lockout should expire")
	}
}

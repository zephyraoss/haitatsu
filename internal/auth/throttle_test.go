package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
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

func openThrottleStore(t *testing.T) *database.Client {
	t.Helper()
	ctx := context.Background()
	dbClient, err := database.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "haitatsu.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbClient.Close() })
	if err := dbClient.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	return dbClient
}

func TestFailureThrottleSharedStoreAccumulatesAcrossNodes(t *testing.T) {
	store := openThrottleStore(t)
	nodeA := NewFailureThrottle(4, time.Minute, time.Minute).WithStore(store.Ent())
	nodeB := NewFailureThrottle(4, time.Minute, time.Minute).WithStore(store.Ent())
	nodeA.RecordFailure("k")
	nodeB.RecordFailure("k")
	nodeA.RecordFailure("k")
	if nodeA.Blocked("k") || nodeB.Blocked("k") {
		t.Fatal("blocked before shared count reached max")
	}
	nodeB.RecordFailure("k")
	if !nodeB.Blocked("k") {
		t.Fatal("node B should be blocked once shared count reaches max")
	}
	if !nodeA.Blocked("k") {
		t.Fatal("node A should observe the shared lockout")
	}
	fresh := NewFailureThrottle(4, time.Minute, time.Minute).WithStore(store.Ent())
	if !fresh.Blocked("k") {
		t.Fatal("fresh node should observe the shared lockout")
	}
	nodeA.RecordSuccess("k")
	if NewFailureThrottle(4, time.Minute, time.Minute).WithStore(store.Ent()).Blocked("k") {
		t.Fatal("success should clear the shared lockout")
	}
}

func TestFailureThrottleBlockedFailsClosedOnStoreError(t *testing.T) {
	store := openThrottleStore(t)
	throttle := NewFailureThrottle(4, time.Minute, time.Minute).WithStore(store.Ent())
	if throttle.Blocked("k") {
		t.Fatal("missing row should not block")
	}
	store.Close()
	if !throttle.Blocked("k") {
		t.Fatal("store error should fail closed")
	}
}

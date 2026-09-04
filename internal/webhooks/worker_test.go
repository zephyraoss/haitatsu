package webhooks

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestFailSchedulesRetryWithBackoff(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	cfg := config.WebhookConfig{MaxAttempts: 3}
	worker := NewWorker(nil, client, func() config.WebhookConfig { return cfg }, metrics.New(), "w", database.BackendSQLite)
	row, err := client.EventLog.Create().SetEventType("message.received").SetPayload(map[string]any{}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job := eventJob{ID: row.ID, EventType: "message.received"}
	if err := worker.fail(ctx, cfg, job, errFake); err != nil {
		t.Fatal(err)
	}
	after, _ := client.EventLog.Get(ctx, row.ID)
	if after.Status != "retry" || after.NextAttemptAt == nil || after.Attempts != 1 {
		t.Fatalf("first failure = %+v", after)
	}
	job.Attempts = 2
	if err := worker.fail(ctx, cfg, job, errFake); err != nil {
		t.Fatal(err)
	}
	after, _ = client.EventLog.Get(ctx, row.ID)
	if after.Status != "failed" {
		t.Fatalf("exhausted attempts should fail: %+v", after)
	}
}

func TestSignatureIsStable(t *testing.T) {
	a := signature("s", "2025-01-01T00:00:00Z", []byte(`{"a":1}`))
	b := signature("s", "2025-01-01T00:00:00Z", []byte(`{"a":1}`))
	c := signature("other", "2025-01-01T00:00:00Z", []byte(`{"a":1}`))
	if a != b || a == c || len(a) < 20 {
		t.Fatal("signature mismatch")
	}
}

func TestClaimSQLiteWebhookJob(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewClient(t)
	created, err := client.EventLog.Create().SetEventType("message.received").SetTraceID("trace").SetPayload(map[string]any{"id": "message"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(db, client, func() config.WebhookConfig { return config.WebhookConfig{} }, metrics.New(), "worker", database.BackendSQLite)
	job, ok, err := worker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || job.ID != created.ID || job.Payload["id"] != "message" {
		t.Fatalf("claimed = %+v, ok = %v", job, ok)
	}
}

type fakeError struct{}

func (fakeError) Error() string { return "boom" }

var errFake = fakeError{}

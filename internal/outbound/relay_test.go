package outbound

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type recordingNotifier struct{ calls int }

func (n *recordingNotifier) OutboundFailure(context.Context, string, string, map[string]any) error {
	n.calls++
	return nil
}

func TestRelayRetryPolicyBacksOffExponentially(t *testing.T) {
	policy := config.RelayConfig{}.RetryPolicy()
	if policy.MaxAttempts != 30 {
		t.Fatalf("default max attempts = %d", policy.MaxAttempts)
	}
	delays := []time.Duration{policy.Delay(0), policy.Delay(1), policy.Delay(2), policy.Delay(10)}
	if delays[0] != time.Minute || delays[1] != 2*time.Minute || delays[2] != 4*time.Minute || delays[3] != 4*time.Hour {
		t.Fatalf("unexpected delays %v", delays)
	}
	var total time.Duration
	for attempt := range policy.MaxAttempts {
		total += policy.Delay(attempt)
	}
	if total < 48*time.Hour {
		t.Fatalf("retry window %v shorter than two days", total)
	}
}

func TestClassifySMTPError(t *testing.T) {
	permanent := classifySMTPError(errors.New("550 5.1.1 user unknown"))
	if permanent.Classification != "permanent" || permanent.Code != 550 || permanent.EnhancedStatusCode != "5.1.1" {
		t.Fatalf("unexpected %+v", permanent)
	}
	temporary := classifySMTPError(errors.New("451 4.7.1 try later"))
	if temporary.Classification != "temporary" || temporary.Code != 451 {
		t.Fatalf("unexpected %+v", temporary)
	}
	network := classifySMTPError(errors.New("dial tcp: connection refused"))
	if network.Classification != "temporary" || network.Code != 0 {
		t.Fatalf("unexpected %+v", network)
	}
}

func TestFinishSchedulesRetryAndEventuallyFails(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	notifier := &recordingNotifier{}
	cfg := config.RelayConfig{Addr: "relay:25", MaxAttempts: 2}
	worker := NewWorker(nil, client, testutil.NewFakeStore(), func() config.RelayConfig { return cfg }, metrics.New(), notifier, "w1", database.BackendSQLite)
	job, err := client.OutboundJob.Create().SetMailboxID("m").SetMessageID("msg").SetReturnPath("bounces+msg@example.test").SetRecipients([]string{"a@x.test"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimedJob{ID: job.ID, MailboxID: "m", MessageID: "msg", Attempts: 0}
	if err := worker.finish(ctx, claimed, deliveryResult{Classification: "temporary", Response: "451"}); err != nil {
		t.Fatal(err)
	}
	after, _ := client.OutboundJob.Get(ctx, job.ID)
	if after.Status != "retry" || after.Attempts != 1 || after.NextAttemptAt == nil {
		t.Fatalf("first temporary failure should schedule retry: %+v", after)
	}
	if notifier.calls != 0 {
		t.Fatal("must not notify before exhausting retries")
	}
	claimed.Attempts = 1
	if err := worker.finish(ctx, claimed, deliveryResult{Classification: "temporary", Response: "451"}); err != nil {
		t.Fatal(err)
	}
	after, _ = client.OutboundJob.Get(ctx, job.ID)
	if after.Status != "failed" || notifier.calls != 1 {
		t.Fatalf("exhausted retries should fail and notify: %+v calls=%d", after, notifier.calls)
	}
}

func TestClaimSQLiteOutboundJob(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewClient(t)
	created, err := client.OutboundJob.Create().
		SetMailboxID("mailbox").
		SetMessageID("message").
		SetReturnPath("sender@example.test").
		SetRecipients([]string{"recipient@example.test"}).
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(db, client, testutil.NewFakeStore(), func() config.RelayConfig { return config.RelayConfig{} }, metrics.New(), nil, "worker", database.BackendSQLite)
	job, ok, err := worker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || job.ID != created.ID || len(job.Recipients) != 1 {
		t.Fatalf("claimed = %+v, ok = %v", job, ok)
	}
}

func TestClaimLibSQLRemoteOutboundJob(t *testing.T) {
	endpoint := os.Getenv("HAITATSU_TEST_LIBSQL_URL")
	if endpoint == "" {
		t.Skip("HAITATSU_TEST_LIBSQL_URL is not set")
	}
	ctx := context.Background()
	dbClient, err := database.Open(ctx, config.DatabaseConfig{
		Driver:    "libsql",
		DSN:       endpoint,
		AuthToken: os.Getenv("HAITATSU_TEST_LIBSQL_AUTH_TOKEN"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dbClient.Close()
	if err := dbClient.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := dbClient.Ent().OutboundJob.Create().
		SetMailboxID("mailbox").
		SetMessageID("message").
		SetReturnPath("sender@example.test").
		SetRecipients([]string{"recipient@example.test"}).
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(dbClient.SQL(), dbClient.Ent(), testutil.NewFakeStore(), func() config.RelayConfig { return config.RelayConfig{} }, metrics.New(), nil, "worker", database.BackendLibSQL)
	job, ok, err := worker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || job.ID != created.ID {
		t.Fatalf("claimed = %+v, ok = %v", job, ok)
	}
}

func TestDeliverUsesInjectedSender(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	blobs := testutil.NewFakeStore()
	_ = blobs.PutMessage(ctx, "k", []byte("From: a@b.test\r\n\r\nhi"))
	if _, err := client.Message.Create().SetID("msg").SetTraceID("t").SetBlobKey("k").SetSha256("s").SetSizeBytes(10).SetFromAddresses([]string{"a@b.test"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	var gotFrom string
	var gotRcpt []string
	worker := NewWorker(nil, client, blobs, func() config.RelayConfig { return config.RelayConfig{Addr: "x"} }, metrics.New(), nil, "w", database.BackendSQLite).WithSender(func(_ context.Context, _ config.RelayConfig, from string, rcpt []string, _ []byte) error {
		gotFrom, gotRcpt = from, rcpt
		return nil
	})
	result := worker.deliver(ctx, claimedJob{MessageID: "msg", ReturnPath: "bounces+msg@b.test", Recipients: []string{"c@d.test"}})
	if result.Classification != "success" || gotFrom != "bounces+msg@b.test" || len(gotRcpt) != 1 {
		t.Fatalf("unexpected %+v from=%s rcpt=%v", result, gotFrom, gotRcpt)
	}
	missing := worker.deliver(ctx, claimedJob{MessageID: "nope", Recipients: []string{"c@d.test"}})
	if missing.Classification != "permanent" {
		t.Fatalf("missing message should fail permanently: %+v", missing)
	}
	_ = ent.IsNotFound
}

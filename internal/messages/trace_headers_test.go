package messages_test

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/spam"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestDeliverStripsForgedTraceHeaders(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "bob@example.test")
	svc := messages.NewService(client, testutil.NewFakeStore(), store, nil, nil, metrics.New(), "mx.test", "node")
	raw := []byte("Authentication-Results: mx.test;\r\n dkim=pass\r\nX-Haitatsu-Trace-ID: forged\r\nSubject: hi\r\n\r\nbody\r\n")
	msg, err := svc.Deliver(ctx, raw, []routing.Result{{Mailboxes: []*ent.Mailbox{mbox}}}, spam.Assessment{Header: "mx.test; spf=fail"})
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Headers["Authentication-Results"]; len(got) != 1 || got[0] != "mx.test; spf=fail" {
		t.Fatalf("Authentication-Results = %q", got)
	}
	if got := msg.Headers["X-Haitatsu-Trace-Id"]; len(got) != 1 || got[0] == "forged" {
		t.Fatalf("X-Haitatsu-Trace-ID = %q", got)
	}
}

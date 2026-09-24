package spam

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestCheckFailsClosedWhenSenderRuleLookupErrors(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	alice := testutil.SeedMailbox(t, testutil.NewMailStore(t, client), "alice@example.test")
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	checker := NewChecker(client, nil, "mx.test")
	recipients := []routing.Result{{OriginalRecipient: alice.PrimaryAddress, BaseRecipient: alice.PrimaryAddress, Mailboxes: []*ent.Mailbox{alice}}}
	raw := []byte("From: sender@remote.test\r\nSubject: hello\r\nMessage-ID: <a@remote.test>\r\n\r\nbody\r\n")

	if _, err := checker.Check(ctx, raw, SMTPContext{RemoteIP: "203.0.113.9", HELO: "remote.test"}, recipients); err == nil {
		t.Fatal("Check returned nil error with unavailable sender-rule store")
	}
}

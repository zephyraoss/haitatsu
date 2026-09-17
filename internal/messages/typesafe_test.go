package messages_test

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/rules"
	"github.com/zephyraoss/haitatsu/internal/spam"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type contentEvaluator struct {
	calls  int
	change func()
}

func (e *contentEvaluator) Evaluate(_ context.Context, _ config.TypeSafeConfig, _ map[string]any) spam.TypeSafeEvaluation {
	e.calls++
	if e.change != nil {
		e.change()
	}
	yes, no := 1.0, 0.0
	return spam.TypeSafeEvaluation{Status: "ok", Model: "test-model", SpamProbability: &yes, PhishingProbability: &no}
}

func TestTypeSafeDeliveryAndReload(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	alice := testutil.SeedMailbox(t, store, "alice@example.test")
	bob := testutil.SeedMailbox(t, store, "bob@example.test")
	recipients := []routing.Result{{OriginalRecipient: alice.PrimaryAddress, BaseRecipient: alice.PrimaryAddress, Mailboxes: []*ent.Mailbox{alice}}, {OriginalRecipient: bob.PrimaryAddress, BaseRecipient: bob.PrimaryAddress, Mailboxes: []*ent.Mailbox{bob}}}
	// An IP allow rule avoids DNS in this test and exercises allowlist semantics.
	if _, err := client.SenderRule.Create().SetScope("mailbox").SetScopeRef(alice.ID).SetKind("allow").SetMatchType("ip").SetValue("127.0.0.1").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RoutingRule.Create().SetScope("mailbox").SetScopeRef(bob.ID).SetName("file mail").SetConditions(map[string]any{}).SetActions([]map[string]any{{"type": "move", "folder": "Archive"}}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	holder := config.NewHolder(&config.Config{Spam: config.SpamConfig{TypeSafe: config.TypeSafeConfig{Mode: "junk", APIKey: "test"}}})
	evaluator := &contentEvaluator{}
	evaluator.change = func() {
		holder.Set(&config.Config{Spam: config.SpamConfig{TypeSafe: config.TypeSafeConfig{Mode: "off"}}})
	}
	checker := spam.NewChecker(client, func() config.SpamConfig { return holder.Get().Spam }, "mx.test", spam.WithTypeSafe(evaluator, nil))
	raw := []byte("Subject: content test\r\nMessage-ID: <test@local>\r\n\r\nA message with content to evaluate.\r\n")
	assessment := checker.Check(ctx, raw, spam.SMTPContext{RemoteIP: "127.0.0.1"}, recipients)
	if evaluator.calls != 1 || !assessment.Junk || assessment.Reject || assessment.Score != 0 {
		t.Fatalf("calls=%d assessment=%+v", evaluator.calls, assessment)
	}
	svc := messages.NewService(client, testutil.NewFakeStore(), store, nil, rules.New(client, store), metrics.New(), "mx.test", "node")
	message, err := svc.Deliver(ctx, raw, recipients, assessment)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.Message.Get(ctx, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := saved.AuthResults["typesafe"].(map[string]any)
	if !ok || result["mode"] != "junk" || result["applied_junk"] != true || saved.SpamScore != 0 {
		t.Fatalf("persisted result: %+v", saved)
	}
	for _, target := range []struct {
		mailbox *ent.Mailbox
		folder  string
	}{{alice, "Junk"}, {bob, "Archive"}} {
		folder, err := store.FolderByName(ctx, target.mailbox.ID, target.folder)
		if err != nil {
			t.Fatal(err)
		}
		items, err := store.ActiveMessagesInFolder(ctx, folder.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("%s in %s: %d messages", target.mailbox.PrimaryAddress, target.folder, len(items))
		}
	}
	next := checker.Check(ctx, raw, spam.SMTPContext{RemoteIP: "127.0.0.1"}, recipients)
	if evaluator.calls != 1 || next.Junk {
		t.Fatalf("reload not applied: calls=%d result=%+v", evaluator.calls, next)
	}
	if _, ok := next.AuthResults["typesafe"]; ok {
		t.Fatal("disabled assessment has typesafe result")
	}
}

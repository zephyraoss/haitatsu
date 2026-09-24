package messages_test

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/spam"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type mailboxContentEvaluator struct{ calls int }

func (e *mailboxContentEvaluator) Evaluate(context.Context, config.TypeSafeConfig, map[string]any) spam.TypeSafeEvaluation {
	e.calls++
	yes, no := 0.9, 0.0
	return spam.TypeSafeEvaluation{Status: "ok", SpamProbability: &yes, PhishingProbability: &no}
}

func TestAliasDeliveryUsesMailboxTypeSafeThresholds(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	alice := testutil.SeedMailbox(t, store, "alice@example.test")
	bob := testutil.SeedMailbox(t, store, "bob@example.test")
	if _, err := client.Mailbox.UpdateOneID(alice.ID).SetSpamThresholds(map[string]float64{"spam_threshold": 0.9}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Route.Create().SetSourceAddress("team@example.test").SetType("alias").SetDestinations([]string{alice.ID, bob.ID}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := routing.NewResolver(client).Resolve(ctx, "team+news@example.test")
	if err != nil || !ok {
		t.Fatalf("resolve alias: ok=%v err=%v", ok, err)
	}
	evaluator := &mailboxContentEvaluator{}
	checker := spam.NewChecker(client, func() config.SpamConfig {
		return config.SpamConfig{TypeSafe: config.TypeSafeConfig{Mode: "junk"}}
	}, "mx.test", spam.WithTypeSafe(evaluator, nil))
	raw := []byte("Subject: alias mail\r\nMessage-ID: <alias@local>\r\n\r\nContent to assess.\r\n")
	recipients := []routing.Result{resolved}
	a, err := checker.Check(ctx, raw, spam.SMTPContext{}, recipients)
	if err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 1 || a.Junk || a.Score != 2 {
		t.Fatalf("calls=%d assessment=%+v", evaluator.calls, a)
	}
	svc := messages.NewService(client, testutil.NewFakeStore(), store, nil, nil, metrics.New(), "mx.test", "node")
	msg, err := svc.Deliver(ctx, raw, recipients, a)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.Message.Get(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	decisions, ok := saved.AuthResults["mailboxes"].(map[string]any)
	if !ok {
		t.Fatalf("missing stored mailbox decisions: %+v", saved.AuthResults)
	}
	for _, target := range []struct {
		mailbox   *ent.Mailbox
		folder    string
		junk      bool
		threshold float64
	}{{alice, "Junk", true, 0.9}, {bob, "INBOX", false, 0.98}} {
		folder, err := store.FolderByName(ctx, target.mailbox.ID, target.folder)
		if err != nil {
			t.Fatal(err)
		}
		items, err := store.ActiveMessagesInFolder(ctx, folder.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].MessageID != msg.ID || items[0].PlusTag != "news" {
			t.Fatalf("%s delivery: %+v", target.mailbox.PrimaryAddress, items)
		}
		decision := decisions[target.mailbox.ID].(map[string]any)
		content := decision["typesafe"].(map[string]any)
		if decision["junk"] != target.junk || content["applied_junk"] != target.junk || content["spam_threshold"] != target.threshold {
			t.Fatalf("%s persisted decision: %+v", target.mailbox.PrimaryAddress, decision)
		}
	}
}

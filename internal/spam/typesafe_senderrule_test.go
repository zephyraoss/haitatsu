package spam

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

// The existing sender-rule behavior is explicit in the integration spec: a
// global rule and recipient mailbox rules are combined in that order and the
// first match wins for every recipient, and an allow rule only reduces the
// traditional score. It does not exempt the message from content filtering.
func TestSenderRulePrecedenceIsGlobalFirstAndAllowDoesNotBypassTypeSafe(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	alice := testutil.SeedMailbox(t, testutil.NewMailStore(t, client), "alice@example.test")
	bob := testutil.SeedMailbox(t, testutil.NewMailStore(t, client), "bob@example.test")

	if _, err := client.SenderRule.Create().SetScope("global").SetKind("block").SetMatchType("ip").SetValue("203.0.113.9").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SenderRule.Create().SetScope("mailbox").SetScopeRef(alice.ID).SetKind("allow").SetMatchType("ip").SetValue("203.0.113.9").Save(ctx); err != nil {
		t.Fatal(err)
	}

	evaluator := &policyEvaluator{result: TypeSafeEvaluation{Status: "ok", SpamProbability: policyProbability(1), PhishingProbability: policyProbability(0)}}
	checker := NewChecker(client, func() config.SpamConfig {
		return config.SpamConfig{TypeSafe: config.TypeSafeConfig{Mode: "junk", APIKey: "k"}}
	}, "mx.test", WithTypeSafe(evaluator, nil))

	recipients := []routing.Result{
		{OriginalRecipient: alice.PrimaryAddress, BaseRecipient: alice.PrimaryAddress, Mailboxes: []*ent.Mailbox{alice}},
		{OriginalRecipient: bob.PrimaryAddress, BaseRecipient: bob.PrimaryAddress, Mailboxes: []*ent.Mailbox{bob}},
	}
	raw := []byte("From: sender@remote.test\r\nSubject: hello\r\nMessage-ID: <a@remote.test>\r\n\r\nbody\r\n")
	assessment, err := checker.Check(ctx, raw, SMTPContext{RemoteIP: "203.0.113.9", HELO: "remote.test"}, recipients)
	if err != nil {
		t.Fatal(err)
	}

	if kind, _ := assessment.AuthResults["list_kind"].(string); kind != "block" {
		t.Fatalf("list_kind = %q, want block (global precedes mailbox rules)", kind)
	}
	if evaluator.calls != 0 {
		t.Fatalf("calls = %d, want 0: a rejected message must skip TypeSafe", evaluator.calls)
	}
	result, ok := assessment.AuthResults["typesafe"].(TypeSafeResult)
	if !ok || result.Status != "skipped_existing_reject" {
		t.Fatalf("typesafe result = %+v", assessment.AuthResults["typesafe"])
	}
}

func TestSenderAllowRuleStillRunsTypeSafeOnceForAllRecipients(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	alice := testutil.SeedMailbox(t, store, "alice@example.test")
	bob := testutil.SeedMailbox(t, store, "bob@example.test")

	if _, err := client.SenderRule.Create().SetScope("mailbox").SetScopeRef(alice.ID).SetKind("allow").SetMatchType("ip").SetValue("127.0.0.1").Save(ctx); err != nil {
		t.Fatal(err)
	}

	evaluator := &policyEvaluator{result: TypeSafeEvaluation{Status: "ok", SpamProbability: policyProbability(0.99), PhishingProbability: policyProbability(0)}}
	checker := NewChecker(client, func() config.SpamConfig {
		return config.SpamConfig{TypeSafe: config.TypeSafeConfig{Mode: "junk", APIKey: "k"}}
	}, "mx.test", WithTypeSafe(evaluator, nil))

	recipients := []routing.Result{
		{OriginalRecipient: alice.PrimaryAddress, BaseRecipient: alice.PrimaryAddress, Mailboxes: []*ent.Mailbox{alice}},
		{OriginalRecipient: bob.PrimaryAddress, BaseRecipient: bob.PrimaryAddress, Mailboxes: []*ent.Mailbox{bob}},
	}
	raw := []byte("From: sender@remote.test\r\nSubject: hello\r\nMessage-ID: <a@remote.test>\r\n\r\nbody\r\n")
	assessment, err := checker.Check(ctx, raw, SMTPContext{RemoteIP: "127.0.0.1", HELO: "remote.test"}, recipients)
	if err != nil {
		t.Fatal(err)
	}

	if evaluator.calls != 1 {
		t.Fatalf("calls = %d, want 1 regardless of recipient count", evaluator.calls)
	}
	if !assessment.Junk {
		t.Fatal("allow rule should not exempt the message from content filtering")
	}
	if assessment.Reject || assessment.Score != 0 {
		t.Fatalf("allow rule should only lower the traditional score: %+v", assessment)
	}
}

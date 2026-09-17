package spam

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

func TestMailboxThresholdPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode, status            string
		globalJunk                    float64
		overrides                     map[string]float64
		spam, phishing                float64
		forced, reject                bool
		wantAlice, wantBob, wantWould bool
		calls                         int
	}{
		{name: "score boundary", overrides: map[string]float64{"junk_threshold": 2}, wantAlice: true},
		{name: "explicit zero", overrides: map[string]float64{"junk_threshold": 0}, wantAlice: true},
		{name: "inherited defaults"},
		{name: "spam boundary", mode: "junk", status: "ok", overrides: map[string]float64{"spam_threshold": 0.9}, spam: 0.9, wantAlice: true, wantWould: true, calls: 1},
		{name: "phishing boundary", mode: "junk", status: "ok", overrides: map[string]float64{"phishing_threshold": 0.8}, phishing: 0.8, wantAlice: true, wantWould: true, calls: 1},
		{name: "higher score threshold allows evaluation", mode: "junk", status: "ok", globalJunk: 1, overrides: map[string]float64{"junk_threshold": 3}, spam: 0.4, wantBob: true, calls: 1},
		{name: "all baseline junk skips evaluation", mode: "junk", globalJunk: 1, wantAlice: true, wantBob: true},
		{name: "override above global probability", mode: "junk", status: "ok", overrides: map[string]float64{"spam_threshold": 1}, spam: 0.99, wantBob: true, calls: 1},
		{name: "shadow records only", mode: "shadow", status: "ok", overrides: map[string]float64{"spam_threshold": 0.9}, spam: 0.9, wantWould: true, calls: 1},
		{name: "failure preserves mailbox baseline", mode: "junk", status: "error_timeout", overrides: map[string]float64{"junk_threshold": 2}, wantAlice: true, calls: 1},
		{name: "forced junk cannot be relaxed", mode: "junk", forced: true, overrides: map[string]float64{"junk_threshold": 100}, wantAlice: true, wantBob: true},
		{name: "reject skips evaluation", mode: "junk", reject: true, overrides: map[string]float64{"junk_threshold": 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.SpamConfig{JunkThreshold: tc.globalJunk, TypeSafe: config.TypeSafeConfig{Mode: tc.mode}}
			e := &policyEvaluator{result: TypeSafeEvaluation{Status: tc.status, SpamProbability: &tc.spam, PhishingProbability: &tc.phishing}}
			observed := 0
			checker := &Checker{typesafe: e, observeTypeSafe: func(TypeSafeResult) { observed++ }}
			a := Assessment{Score: 2, Junk: tc.forced || 2 >= junkThreshold(cfg), Reject: tc.reject, Reasons: []string{"missing_from"}, AuthResults: map[string]any{"spam_reasons": []string{"missing_from"}}}
			alice := &ent.Mailbox{ID: "alice", SpamThresholds: tc.overrides}
			bob := &ent.Mailbox{ID: "bob"}
			recipients := []routing.Result{{Mailboxes: []*ent.Mailbox{alice, bob, alice}}}
			checker.applyMailboxPolicy(context.Background(), []byte("Subject: test\r\n\r\nMessage text."), cfg, recipients, tc.forced, &a)
			if a.JunkForMailbox("alice") != tc.wantAlice || a.JunkForMailbox("bob") != tc.wantBob || a.Score != 2 || a.Reject != tc.reject {
				t.Fatalf("unexpected assessment: %+v mailboxes=%+v", a, a.Mailboxes)
			}
			if e.calls != tc.calls || len(a.Mailboxes) != 2 {
				t.Fatalf("calls=%d want=%d mailboxes=%d", e.calls, tc.calls, len(a.Mailboxes))
			}
			if tc.mode == "" {
				if observed != 0 || a.Mailboxes["alice"].TypeSafe != nil {
					t.Fatal("disabled TypeSafe produced results")
				}
			} else {
				if observed != 1 || a.Mailboxes["alice"].TypeSafe.WouldJunk != tc.wantWould {
					t.Fatalf("observed=%d result=%+v", observed, a.Mailboxes["alice"].TypeSafe)
				}
			}
			if _, err := json.Marshal(a.AuthResults); err != nil {
				t.Fatalf("metadata cannot be persisted: %v", err)
			}
		})
	}
}

type changingMailboxEvaluator struct{ change func() }

func (e changingMailboxEvaluator) Evaluate(context.Context, config.TypeSafeConfig, map[string]any) TypeSafeEvaluation {
	e.change()
	return TypeSafeEvaluation{Status: "ok", SpamProbability: policyProbability(0.9), PhishingProbability: policyProbability(0)}
}

func TestMailboxThresholdSnapshot(t *testing.T) {
	alice := &ent.Mailbox{ID: "alice", SpamThresholds: map[string]float64{"spam_threshold": 0.8}}
	checker := &Checker{typesafe: changingMailboxEvaluator{change: func() { alice.SpamThresholds["spam_threshold"] = 1 }}}
	cfg := config.SpamConfig{TypeSafe: config.TypeSafeConfig{Mode: "junk"}}
	recipients := []routing.Result{{Mailboxes: []*ent.Mailbox{alice}}}
	first := Assessment{}
	checker.applyMailboxPolicy(context.Background(), []byte("Subject: test\r\n\r\nMessage text."), cfg, recipients, false, &first)
	if !first.JunkForMailbox("alice") || first.Mailboxes["alice"].TypeSafe.SpamThreshold != 0.8 {
		t.Fatalf("in-flight settings changed: %+v", first.Mailboxes["alice"])
	}
	next := Assessment{}
	checker.applyMailboxPolicy(context.Background(), []byte("Subject: test\r\n\r\nMessage text."), cfg, recipients, false, &next)
	if next.JunkForMailbox("alice") || next.Mailboxes["alice"].TypeSafe.SpamThreshold != 1 {
		t.Fatalf("new settings not used: %+v", next.Mailboxes["alice"])
	}
	if !(Assessment{Junk: true}).JunkForMailbox("unknown") || (Assessment{}).JunkForMailbox("unknown") {
		t.Fatal("message-wide assessment fallback changed")
	}
}

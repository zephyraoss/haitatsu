package spam

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
)

type policyEvaluator struct {
	calls  int
	result TypeSafeEvaluation
}

func (e *policyEvaluator) Evaluate(_ context.Context, _ config.TypeSafeConfig, _ map[string]any) TypeSafeEvaluation {
	e.calls++
	return e.result
}
func policyProbability(v float64) *float64 { return &v }

func TestTypeSafePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode, status           string
		spam, phishing               *float64
		baselineJunk, baselineReject bool
		wantJunk, wantWould          bool
		wantCalls                    int
	}{
		{name: "off", mode: "off", status: "ok", spam: policyProbability(1), phishing: policyProbability(1)},
		{name: "shadow", mode: "shadow", status: "ok", spam: policyProbability(1), phishing: policyProbability(0), wantWould: true, wantCalls: 1},
		{name: "spam", mode: "junk", status: "ok", spam: policyProbability(.98), phishing: policyProbability(0), wantJunk: true, wantWould: true, wantCalls: 1},
		{name: "phishing", mode: "junk", status: "ok", spam: policyProbability(0), phishing: policyProbability(1), wantJunk: true, wantWould: true, wantCalls: 1},
		{name: "uncertain", mode: "junk", status: "ok", spam: policyProbability(.5), phishing: policyProbability(.5), wantCalls: 1},
		{name: "failure", mode: "junk", status: "error_timeout", wantCalls: 1},
		{name: "missing", mode: "junk", status: "ok", spam: policyProbability(1), wantCalls: 1},
		{name: "nan", mode: "junk", status: "ok", spam: policyProbability(math.NaN()), phishing: policyProbability(0), wantCalls: 1},
		{name: "existing junk", mode: "junk", baselineJunk: true, wantJunk: true},
		{name: "existing reject", mode: "junk", baselineReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &policyEvaluator{result: TypeSafeEvaluation{Status: tc.status, SpamProbability: tc.spam, PhishingProbability: tc.phishing}}
			observed := 0
			c := &Checker{typesafe: e, observeTypeSafe: func(TypeSafeResult) { observed++ }}
			a := Assessment{Score: 3, Header: "mx; spf=pass", Reasons: []string{"original"}, AuthResults: map[string]any{"spam_reasons": []string{"original"}}, Junk: tc.baselineJunk, Reject: tc.baselineReject}
			cfg := config.TypeSafeConfig{Mode: tc.mode}.WithDefaults()
			c.applyTypeSafe(context.Background(), []byte("From: Sender <sender@example.test>\r\nSubject: hello\r\n\r\nThis is the message body."), cfg, &a)
			if a.Junk != tc.wantJunk || a.Reject != tc.baselineReject || a.Score != 3 || a.Header != "mx; spf=pass" {
				t.Fatalf("assessment: %+v", a)
			}
			if e.calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", e.calls, tc.wantCalls)
			}
			if tc.mode == "off" {
				if _, ok := a.AuthResults["typesafe"]; ok || observed != 0 {
					t.Fatal("disabled produced metadata")
				}
				return
			}
			result := a.AuthResults["typesafe"].(TypeSafeResult)
			if result.WouldJunk != tc.wantWould || result.AppliedJunk != (tc.wantWould && tc.mode == "junk") || observed != 1 {
				t.Fatalf("result: %+v", result)
			}
			if _, err := json.Marshal(a.AuthResults); err != nil {
				t.Fatal(err)
			}
			if !result.AppliedJunk && !reflect.DeepEqual(a.Reasons, []string{"original"}) {
				t.Fatalf("changed baseline reasons: %v", a.Reasons)
			}
			if result.AppliedJunk && !reflect.DeepEqual(a.Reasons, a.AuthResults["spam_reasons"]) {
				t.Fatal("stored reasons diverged")
			}
		})
	}
}

func TestTypeSafeSkipsUnusableContent(t *testing.T) {
	e := &policyEvaluator{}
	c := &Checker{typesafe: e}
	a := Assessment{}
	c.applyTypeSafe(context.Background(), []byte("Subject: blank\r\n\r\n"), config.TypeSafeConfig{Mode: "junk"}.WithDefaults(), &a)
	if e.calls != 0 || a.Junk || a.AuthResults["typesafe"].(TypeSafeResult).Status != "skipped_no_content" {
		t.Fatalf("calls=%d assessment=%+v", e.calls, a)
	}
}

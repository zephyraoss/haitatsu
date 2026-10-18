package spam

import (
	"testing"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dmarc"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

func TestScoreDMARCPolicyOnlyOnFailure(t *testing.T) {
	t.Run("pass does not add policy points", func(t *testing.T) {
		got, reasons := score(authres.ResultPass, authres.ResultPass, authres.ResultPass, dmarc.PolicyReject, "")
		if got != 0 {
			t.Fatalf("score = %v, want 0", got)
		}
		if len(reasons) != 0 {
			t.Fatalf("reasons = %v, want none", reasons)
		}
	})

	t.Run("fail with reject policy", func(t *testing.T) {
		got, reasons := score(authres.ResultNone, authres.ResultNone, authres.ResultFail, dmarc.PolicyReject, "")
		if got != 13 {
			t.Fatalf("score = %v, want 13", got)
		}
		for _, want := range []string{"dmarc_fail", "dmarc_reject"} {
			if !contains(reasons, want) {
				t.Fatalf("reasons %v missing %q", reasons, want)
			}
		}
	})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestFromAllowRuleRequiresAuthentication(t *testing.T) {
	entry := &ent.SenderRule{Kind: "allow", MatchType: "domain", Value: "bank.test"}
	metadata := mailparse.Metadata{From: []string{"boss@bank.test"}}
	if matches(entry, metadata, SMTPContext{}, false) {
		t.Fatal("unauthenticated from must not match an allow rule")
	}
	if !matches(entry, metadata, SMTPContext{}, true) {
		t.Fatal("authenticated from should match an allow rule")
	}
}

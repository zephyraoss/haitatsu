package spam

import (
	"testing"

	"github.com/emersion/go-msgauth/dmarc"
)

func TestDMARCAlignment(t *testing.T) {
	if !aligned(dmarc.AlignmentRelaxed, "example.com", "mail.example.com") {
		t.Fatal("relaxed alignment should accept subdomains")
	}
	if aligned(dmarc.AlignmentStrict, "example.com", "mail.example.com") {
		t.Fatal("strict alignment must reject subdomains")
	}
	if aligned(dmarc.AlignmentRelaxed, "example.com", "example.org") {
		t.Fatal("different organizational domains must not align")
	}
	if !aligned(dmarc.AlignmentStrict, "Example.com", "example.com") {
		t.Fatal("case insensitive exact match should align")
	}
}

func TestReverseIP(t *testing.T) {
	if got := reverseIP(parseIP("192.0.2.10")); got != "10.2.0.192" {
		t.Fatalf("v4 reverse = %q", got)
	}
	if got := reverseIP(parseIP("2001:db8::1")); got[:8] != "1.0.0.0." {
		t.Fatalf("v6 reverse = %q", got)
	}
}

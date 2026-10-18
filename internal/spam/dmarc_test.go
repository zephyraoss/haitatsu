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

func TestDMARCFromDomainRejectsMixedDomains(t *testing.T) {
	if got := dmarcFromDomain([]string{"a@example.com", "b@example.com"}); got != "example.com" {
		t.Fatalf("same domain = %q", got)
	}
	if got := dmarcFromDomain([]string{"a@example.com", "b@example.org"}); got != "" {
		t.Fatalf("mixed domains = %q", got)
	}
}

func TestDNSBLHitIgnoresErrorCodes(t *testing.T) {
	if !dnsblHit(parseIP("127.0.0.2")) {
		t.Fatal("127.0.0.2 should be listed")
	}
	if dnsblHit(parseIP("127.255.255.254")) {
		t.Fatal("127.255.255.x is an error code, not a listing")
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

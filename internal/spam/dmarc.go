package spam

import (
	"strings"

	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

func aligned(mode dmarc.AlignmentMode, fromDomain string, authDomain string) bool {
	fromDomain = strings.ToLower(strings.TrimSpace(fromDomain))
	authDomain = strings.ToLower(strings.TrimSpace(authDomain))
	if fromDomain == "" || authDomain == "" {
		return false
	}
	if fromDomain == authDomain {
		return true
	}
	if mode == dmarc.AlignmentStrict {
		return false
	}
	return organizationalDomain(fromDomain) == organizationalDomain(authDomain)
}

func organizationalDomain(domain string) string {
	org, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return domain
	}
	return org
}

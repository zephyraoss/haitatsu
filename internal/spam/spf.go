package spam

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/authres"
)

const (
	maxSPFDepth = 10
	// RFC 7208 §4.6.4: at most 10 DNS-querying mechanisms/modifiers per evaluation.
	maxSPFLookups = 10
	// RFC 7208 §4.6.4: at most 10 MX names resolved per mx mechanism.
	maxSPFMXHosts = 10
	spfTimeout    = 20 * time.Second
)

// spfLookups is the shared DNS lookup budget for one SPF evaluation.
type spfLookups struct{ used int }

func (l *spfLookups) take() bool {
	if l.used >= maxSPFLookups {
		return false
	}
	l.used++
	return true
}

func checkSPF(ctx context.Context, smtp SMTPContext) (authres.ResultValue, string, string) {
	domain := addressDomain(smtp.MailFrom)
	if domain == "" {
		domain = strings.ToLower(strings.TrimSpace(smtp.HELO))
	}
	remoteIP := net.ParseIP(smtp.RemoteIP)
	if domain == "" || remoteIP == nil {
		return authres.ResultNone, domain, ""
	}
	ctx, cancel := context.WithTimeout(ctx, spfTimeout)
	defer cancel()
	result, reason := evalSPF(ctx, domain, remoteIP, 0, &spfLookups{})
	return result, domain, reason
}

func evalSPF(ctx context.Context, domain string, remoteIP net.IP, depth int, lookups *spfLookups) (authres.ResultValue, string) {
	if depth > maxSPFDepth {
		return authres.ResultPermError, "too many SPF includes or redirects"
	}
	record, result, reason := lookupSPF(ctx, domain)
	if result != "" {
		return result, reason
	}

	var redirect string
	for _, token := range strings.Fields(record) {
		if strings.EqualFold(token, "v=spf1") {
			continue
		}
		if value, ok := strings.CutPrefix(token, "redirect="); ok {
			redirect = value
			continue
		}

		qualifier, name, arg, cidr := mechanism(token)
		switch name {
		case "all":
			return qualifierResult(qualifier), "all"
		case "include":
			if !lookups.take() {
				return authres.ResultPermError, "too many DNS lookups"
			}
			included, includeReason := evalSPF(ctx, arg, remoteIP, depth+1, lookups)
			if included == authres.ResultPass {
				return qualifierResult(qualifier), "include:" + arg
			}
			if included == authres.ResultTempError || included == authres.ResultPermError {
				return included, includeReason
			}
		case "ip4", "ip6":
			if ipMechanismMatches(remoteIP, arg, cidr) {
				return qualifierResult(qualifier), name + ":" + arg
			}
		case "a":
			if !lookups.take() {
				return authres.ResultPermError, "too many DNS lookups"
			}
			if hostMatches(ctx, remoteIP, mechanismDomain(arg, domain), cidr) {
				return qualifierResult(qualifier), "a"
			}
		case "mx":
			if !lookups.take() {
				return authres.ResultPermError, "too many DNS lookups"
			}
			if mxMatches(ctx, remoteIP, mechanismDomain(arg, domain), cidr) {
				return qualifierResult(qualifier), "mx"
			}
		case "exists", "ptr":
			if !lookups.take() {
				return authres.ResultPermError, "too many DNS lookups"
			}
		}
	}

	if redirect != "" {
		if !lookups.take() {
			return authres.ResultPermError, "too many DNS lookups"
		}
		return evalSPF(ctx, redirect, remoteIP, depth+1, lookups)
	}
	return authres.ResultNeutral, "no SPF mechanism matched"
}

func lookupSPF(ctx context.Context, domain string) (string, authres.ResultValue, string) {
	records, err := net.DefaultResolver.LookupTXT(ctx, domain)
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			return "", authres.ResultNone, "no SPF record"
		}
		return "", authres.ResultTempError, err.Error()
	}
	var spf []string
	for _, record := range records {
		if strings.HasPrefix(strings.ToLower(record), "v=spf1") {
			spf = append(spf, record)
		}
	}
	if len(spf) == 0 {
		return "", authres.ResultNone, "no SPF record"
	}
	if len(spf) > 1 {
		return "", authres.ResultPermError, "multiple SPF records"
	}
	return spf[0], "", ""
}

func mechanism(token string) (byte, string, string, string) {
	qualifier := byte('+')
	if strings.ContainsRune("+-~?", rune(token[0])) {
		qualifier = token[0]
		token = token[1:]
	}
	cidr := ""
	if before, after, ok := strings.Cut(token, "/"); ok {
		token = before
		cidr = after
	}
	name, arg, _ := strings.Cut(token, ":")
	return qualifier, strings.ToLower(name), arg, cidr
}

func qualifierResult(qualifier byte) authres.ResultValue {
	switch qualifier {
	case '-':
		return authres.ResultFail
	case '~':
		return authres.ResultSoftFail
	case '?':
		return authres.ResultNeutral
	default:
		return authres.ResultPass
	}
}

func mechanismDomain(arg string, current string) string {
	if arg == "" {
		return current
	}
	return arg
}

func ipMechanismMatches(remoteIP net.IP, value string, cidr string) bool {
	mechanismIP := net.ParseIP(value)
	if mechanismIP == nil {
		return false
	}
	if cidr == "" {
		return remoteIP.Equal(mechanismIP)
	}
	bits := 32
	if mechanismIP.To4() == nil {
		bits = 128
	}
	ones, err := strconv.Atoi(cidr)
	if err != nil || ones < 0 || ones > bits {
		return false
	}
	network := net.IPNet{IP: mechanismIP.Mask(net.CIDRMask(ones, bits)), Mask: net.CIDRMask(ones, bits)}
	return network.Contains(remoteIP)
}

func hostMatches(ctx context.Context, remoteIP net.IP, domain string, cidr string) bool {
	addresses, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if ipMechanismMatches(remoteIP, address, cidr) {
			return true
		}
	}
	return false
}

func mxMatches(ctx context.Context, remoteIP net.IP, domain string, cidr string) bool {
	records, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err != nil {
		return false
	}
	if len(records) > maxSPFMXHosts {
		records = records[:maxSPFMXHosts]
	}
	for _, record := range records {
		if hostMatches(ctx, remoteIP, strings.TrimSuffix(record.Host, "."), cidr) {
			return true
		}
	}
	return false
}

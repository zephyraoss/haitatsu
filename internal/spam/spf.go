package spam

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/emersion/go-msgauth/authres"
)

const maxSPFDepth = 10

func checkSPF(ctx context.Context, smtp SMTPContext) (authres.ResultValue, string, string) {
	domain := addressDomain(smtp.MailFrom)
	if domain == "" {
		domain = strings.ToLower(strings.TrimSpace(smtp.HELO))
	}
	remoteIP := net.ParseIP(smtp.RemoteIP)
	if domain == "" || remoteIP == nil {
		return authres.ResultNone, domain, ""
	}
	result, reason := evalSPF(ctx, domain, remoteIP, 0)
	return result, domain, reason
}

func evalSPF(ctx context.Context, domain string, remoteIP net.IP, depth int) (authres.ResultValue, string) {
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
			included, includeReason := evalSPF(ctx, arg, remoteIP, depth+1)
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
			if hostMatches(ctx, remoteIP, mechanismDomain(arg, domain), cidr) {
				return qualifierResult(qualifier), "a"
			}
		case "mx":
			if mxMatches(ctx, remoteIP, mechanismDomain(arg, domain), cidr) {
				return qualifierResult(qualifier), "mx"
			}
		}
	}

	if redirect != "" {
		return evalSPF(ctx, redirect, remoteIP, depth+1)
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
	for _, record := range records {
		if hostMatches(ctx, remoteIP, strings.TrimSuffix(record.Host, "."), cidr) {
			return true
		}
	}
	return false
}

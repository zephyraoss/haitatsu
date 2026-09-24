package importexport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

const imapDialTimeout = 30 * time.Second

// IMAPImportPolicy is the operator-controlled policy for outbound IMAP import
// connections. Job sources never override it.
type IMAPImportPolicy struct {
	// AllowedHosts, when non-empty, is the only set of hostnames imports may
	// connect to. Hosts listed here are trusted even when they resolve to
	// private addresses.
	AllowedHosts []string
	// AllowInsecure permits plaintext connections and disabled certificate
	// verification when the job source asks for them.
	AllowInsecure bool
}

func (p IMAPImportPolicy) hostAllowed(host string) bool {
	if len(p.AllowedHosts) == 0 {
		return true
	}
	for _, allowed := range p.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}

func (p IMAPImportPolicy) hostExplicitlyAllowed(host string) bool {
	return len(p.AllowedHosts) > 0 && p.hostAllowed(host)
}

type imapTarget struct {
	host  string
	addrs []netip.AddrPort
}

// resolveIMAPTarget validates a caller-supplied IMAP address against the
// policy and resolves it to concrete IP addresses. The resolved addresses are
// dialed directly so a DNS rebind between validation and connect cannot bypass
// the range checks.
func resolveIMAPTarget(ctx context.Context, addr string, policy IMAPImportPolicy) (imapTarget, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return imapTarget{}, fmt.Errorf("imap import source.addr must be host:port: %w", err)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return imapTarget{}, fmt.Errorf("imap import source.addr must include a host")
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil || port == 0 {
		return imapTarget{}, fmt.Errorf("imap import source.addr has invalid port %q", portText)
	}
	if !policy.hostAllowed(host) {
		return imapTarget{}, fmt.Errorf("imap import host %q is not in workers.imap_import_allowed_hosts", host)
	}
	trusted := policy.hostExplicitlyAllowed(host)

	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return imapTarget{}, fmt.Errorf("resolve imap import host %q: %w", host, err)
		}
		ips = resolved
	}
	if len(ips) == 0 {
		return imapTarget{}, fmt.Errorf("imap import host %q did not resolve", host)
	}

	target := imapTarget{host: host}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !trusted && !isPublicAddr(ip) {
			return imapTarget{}, fmt.Errorf("imap import host %q resolves to disallowed address %s", host, ip)
		}
		target.addrs = append(target.addrs, netip.AddrPortFrom(ip, uint16(port)))
	}
	return target, nil
}

func isPublicAddr(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, block := range blockedPrefixes {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),     // carrier-grade NAT
	netip.MustParsePrefix("169.254.0.0/16"),    // link-local / cloud metadata
	netip.MustParsePrefix("192.0.0.0/24"),      // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),      // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),     // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),   // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),    // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),       // reserved + broadcast
	netip.MustParsePrefix("::/128"),            // unspecified
	netip.MustParsePrefix("::ffff:0:0/96"),     // IPv4-mapped (handled via Unmap, kept for safety)
	netip.MustParsePrefix("64:ff9b::/96"),      // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),    // local-use NAT64
	netip.MustParsePrefix("100::/64"),          // discard
	netip.MustParsePrefix("2001::/23"),         // IETF protocol assignments (incl. Teredo, ORCHID)
	netip.MustParsePrefix("2001:db8::/32"),     // documentation
	netip.MustParsePrefix("fc00::/7"),          // unique local
	netip.MustParsePrefix("fe80::/10"),         // link-local
	netip.MustParsePrefix("fd00:ec2::254/128"), // AWS IPv6 metadata
}

func dialIMAP(ctx context.Context, source map[string]any, policy IMAPImportPolicy) (*imapclient.Client, error) {
	addr := sourceString(source, "addr")
	if addr == "" {
		return nil, fmt.Errorf("imap import requires source.addr")
	}
	target, err := resolveIMAPTarget(ctx, addr, policy)
	if err != nil {
		return nil, err
	}

	startTLS := sourceBool(source, "starttls")
	plaintext := false
	if tlsEnabled, ok := source["tls"].(bool); ok && !tlsEnabled && !startTLS {
		plaintext = true
	}
	skipVerify := sourceBool(source, "skip_verify")
	if (plaintext || skipVerify) && !policy.AllowInsecure {
		return nil, fmt.Errorf("imap import requires verified TLS; plaintext and skip_verify are disabled by workers.imap_import_allow_insecure")
	}

	conn, err := dialResolved(ctx, target)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{ServerName: target.host, InsecureSkipVerify: skipVerify, MinVersion: tls.VersionTLS12}
	options := &imapclient.Options{TLSConfig: tlsConfig}
	switch {
	case startTLS:
		return imapclient.NewStartTLS(conn, options)
	case plaintext:
		return imapclient.New(conn, options), nil
	default:
		tlsConfig.NextProtos = []string{"imap"}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return imapclient.New(tlsConn, options), nil
	}
}

func dialResolved(ctx context.Context, target imapTarget) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: imapDialTimeout}
	var lastErr error
	for _, addrPort := range target.addrs {
		conn, err := dialer.DialContext(ctx, "tcp", addrPort.String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dial imap import host %q: %w", target.host, lastErr)
}

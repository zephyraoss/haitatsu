package importexport

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestResolveIMAPTargetRejectsInternalAddresses(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:143", "10.0.0.5:143", "192.168.1.1:993", "172.16.0.1:993",
		"169.254.169.254:80", "100.64.0.1:143", "0.0.0.0:143", "localhost:143",
		"[::1]:143", "[fd00::1]:143", "[fe80::1]:143", "[::ffff:10.0.0.1]:143",
		"[fd00:ec2::254]:80",
	} {
		if _, err := resolveIMAPTarget(context.Background(), addr, config.ImportsConfig{}); err == nil {
			t.Errorf("%s: expected rejection", addr)
		}
	}
}

func TestResolveIMAPTargetRequiresPort(t *testing.T) {
	if _, err := resolveIMAPTarget(context.Background(), "imap.example.com", config.ImportsConfig{}); err == nil {
		t.Fatal("expected error for missing port")
	}
}

func TestResolveIMAPTargetAllowsPublicIP(t *testing.T) {
	target, err := resolveIMAPTarget(context.Background(), "8.8.8.8:993", config.ImportsConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(target.addrs) != 1 || target.addrs[0].String() != "8.8.8.8:993" {
		t.Fatalf("unexpected addrs %v", target.addrs)
	}
}

func TestResolveIMAPTargetAllowlist(t *testing.T) {
	imports := config.ImportsConfig{AllowedHosts: []string{"Mail.Internal.Example"}}
	if _, err := resolveIMAPTarget(context.Background(), "8.8.8.8:993", imports); err == nil || !strings.Contains(err.Error(), "not in imports.allowed_hosts") {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
	// An explicitly allowlisted host may point at a private address.
	if _, err := resolveIMAPTarget(context.Background(), "10.0.0.5:143", config.ImportsConfig{AllowedHosts: []string{"10.0.0.5"}}); err != nil {
		t.Fatalf("allowlisted private host rejected: %v", err)
	}
}

func TestDialIMAPRejectsInsecureTransportByDefault(t *testing.T) {
	for _, source := range []map[string]any{
		{"addr": "8.8.8.8:143", "tls": false},
		{"addr": "8.8.8.8:993", "skip_verify": true},
		{"addr": "8.8.8.8:143", "starttls": true, "skip_verify": true},
	} {
		_, err := dialIMAP(context.Background(), importJob{ID: "imp", Source: source}, config.ImportsConfig{})
		if err == nil || !strings.Contains(err.Error(), "is not permitted") {
			t.Errorf("%v: expected insecure transport rejection, got %v", source, err)
		}
	}
}

func TestDialIMAPNeverDialsPrivateTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- struct{}{}
			conn.Close()
		}
	}()
	addr := listener.Addr().String()
	job := importJob{ID: "imp", Source: map[string]any{"addr": addr, "tls": false}}
	_, err = dialIMAP(context.Background(), job, config.ImportsConfig{InsecureTLSHosts: []string{"127.0.0.1"}})
	if err == nil || !strings.Contains(err.Error(), "disallowed address") {
		t.Fatalf("expected loopback target to be rejected, got %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("worker connected to loopback target")
	default:
	}
}

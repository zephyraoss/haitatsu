package certs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestManualModeFailsClosedWithoutCertFiles(t *testing.T) {
	for _, mode := range []string{"", "manual"} {
		if _, err := TLSConfig(context.Background(), Options{TLS: config.TLSConfig{Mode: mode}}); err == nil {
			t.Fatalf("mode %q with no cert/key should fail", mode)
		}
		if _, err := TLSConfig(context.Background(), Options{TLS: config.TLSConfig{Mode: mode, CertFile: "cert.pem"}}); err == nil {
			t.Fatalf("mode %q with missing key should fail", mode)
		}
	}
}

func TestOffModeReturnsNil(t *testing.T) {
	cfg, err := TLSConfig(context.Background(), Options{TLS: config.TLSConfig{Mode: "off"}})
	if err != nil || cfg != nil {
		t.Fatalf("expected nil config, got %v %v", cfg, err)
	}
}

func TestManualModeReloadsChangedCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	write := func(hostname string, modTime time.Time) {
		certPEM, keyPEM := selfSigned(t, hostname)
		for path, data := range map[string][]byte{certFile: certPEM, keyFile: keyPEM} {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, modTime, modTime); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("old.example", time.Now().Add(-time.Hour))
	cfg, err := TLSConfig(context.Background(), Options{TLS: config.TLSConfig{Mode: "manual", CertFile: certFile, KeyFile: keyFile}})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cfg.GetCertificate(nil)
	if err != nil || leaf(t, cert).Subject.CommonName != "old.example" {
		t.Fatalf("initial certificate = %v, %v", cert, err)
	}
	write("new.example", time.Now())
	cert, err = cfg.GetCertificate(nil)
	if err != nil || leaf(t, cert).Subject.CommonName != "new.example" {
		t.Fatalf("reloaded certificate = %v, %v", cert, err)
	}
}

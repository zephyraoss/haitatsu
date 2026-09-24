package certs

import (
	"context"
	"testing"

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

package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/caddyserver/certmagic"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TLSConfig(ctx context.Context, cfg config.TLSConfig, publicHostname string) (*tls.Config, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "", "manual":
		return manualTLSConfig(cfg)
	case "acme":
		return acmeTLSConfig(ctx, cfg, publicHostname)
	case "off", "disabled":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported tls mode %q", cfg.Mode)
	}
}

func manualTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	if cfg.CertFile == "" && cfg.KeyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func acmeTLSConfig(ctx context.Context, cfg config.TLSConfig, publicHostname string) (*tls.Config, error) {
	if publicHostname == "" {
		return nil, fmt.Errorf("public hostname is required for ACME TLS")
	}
	magic := certmagic.NewDefault()
	if cfg.ACMECachePath != "" {
		magic.Storage = &certmagic.FileStorage{Path: cfg.ACMECachePath}
	}
	magic.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     acmeCA(cfg.ACMECA),
		Email:  cfg.ACMEEmail,
		Agreed: true,
	})}
	if err := magic.ManageSync(ctx, []string{publicHostname}); err != nil {
		return nil, err
	}
	tlsConfig := magic.TLSConfig()
	tlsConfig.ServerName = publicHostname
	return tlsConfig, nil
}

func acmeCA(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "letsencrypt", "letsencrypt-production":
		return certmagic.LetsEncryptProductionCA
	case "letsencrypt-staging":
		return certmagic.LetsEncryptStagingCA
	case "zerossl":
		return certmagic.ZeroSSLProductionCA
	default:
		return value
	}
}

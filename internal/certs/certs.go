package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/certmagic"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func defaultACMECachePath() string {
	executable, err := os.Executable()
	if err != nil {
		return "./cache/certmagic"
	}
	return filepath.Join(filepath.Dir(executable), "cache", "certmagic")
}

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
	magic.Storage = &certmagic.FileStorage{Path: acmeCachePath(cfg)}
	magic.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:                        acmeCA(cfg.ACMECA),
		Email:                     cfg.ACMEEmail,
		Agreed:                    true,
		ListenHost:                cfg.ACMEListenHost,
		AltHTTPPort:               cfg.ACMEHTTPPort,
		AltTLSALPNPort:            cfg.ACMETLSALPNPort,
		DisableHTTPChallenge:      cfg.ACMEDisableHTTPChallenge,
		DisableTLSALPNChallenge:   cfg.ACMEDisableTLSALPNChallenge,
		DisableDistributedSolvers: cfg.ACMEDisableDistributedSolvers,
	})}
	if err := magic.ManageAsync(ctx, []string{publicHostname}); err != nil {
		return nil, err
	}
	tlsConfig := magic.TLSConfig()
	tlsConfig.ServerName = publicHostname
	return tlsConfig, nil
}

func acmeCachePath(cfg config.TLSConfig) string {
	if path := strings.TrimSpace(cfg.ACMECachePath); path != "" {
		return path
	}
	return defaultACMECachePath()
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

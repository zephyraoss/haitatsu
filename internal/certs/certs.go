package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"

	"github.com/caddyserver/certmagic"

	"github.com/zephyraoss/haitatsu/internal/config"
)

const defaultACMECachePath = "/var/lib/haitatsu/certmagic"

type Options struct {
	TLS            config.TLSConfig
	PublicHostname string
	Hostnames      []string
}

func TLSConfig(ctx context.Context, opts Options) (*tls.Config, error) {
	switch strings.ToLower(strings.TrimSpace(opts.TLS.Mode)) {
	case "", "manual":
		return manualTLSConfig(opts.TLS)
	case "acme":
		return acmeTLSConfig(ctx, opts)
	case "storage":
		return storageTLSConfig(ctx, opts)
	case "off", "disabled":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported tls mode %q", opts.TLS.Mode)
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

func storageTLSConfig(ctx context.Context, opts Options) (*tls.Config, error) {
	hostnames := opts.Hostnames
	source, err := NewS3Source(opts.TLS.Storage)
	if err != nil {
		return nil, err
	}
	store := NewStore(source, hostnames, opts.TLS.Storage.IssuerKey())
	if err := store.Refresh(ctx); err != nil {
		slog.Warn("certificates not yet in storage; TLS handshakes will fail until they appear", "error", err)
	}
	store.RefreshEvery(ctx, opts.TLS.Storage.RefreshInterval())
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: store.GetCertificate,
	}, nil
}

func acmeTLSConfig(ctx context.Context, opts Options) (*tls.Config, error) {
	if opts.PublicHostname == "" {
		return nil, fmt.Errorf("public hostname is required for ACME TLS")
	}
	magic := certmagic.NewDefault()
	magic.Storage = &certmagic.FileStorage{Path: acmeCachePath(opts.TLS)}
	magic.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:                        acmeCA(opts.TLS.ACMECA),
		Email:                     opts.TLS.ACMEEmail,
		Agreed:                    true,
		ListenHost:                opts.TLS.ACMEListenHost,
		AltHTTPPort:               opts.TLS.ACMEHTTPPort,
		AltTLSALPNPort:            opts.TLS.ACMETLSALPNPort,
		DisableHTTPChallenge:      opts.TLS.ACMEDisableHTTPChallenge,
		DisableTLSALPNChallenge:   opts.TLS.ACMEDisableTLSALPNChallenge,
		DisableDistributedSolvers: opts.TLS.ACMEDisableDistributedSolvers,
	})}
	if err := magic.ManageAsync(ctx, []string{opts.PublicHostname}); err != nil {
		return nil, err
	}
	tlsConfig := magic.TLSConfig()
	tlsConfig.ServerName = opts.PublicHostname
	return tlsConfig, nil
}

func acmeCachePath(cfg config.TLSConfig) string {
	if path := strings.TrimSpace(cfg.ACMECachePath); path != "" {
		return path
	}
	return defaultACMECachePath
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

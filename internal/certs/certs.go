package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"

	"github.com/zephyraoss/haitatsu/internal/config"
)

const defaultACMECachePath = "/var/lib/haitatsu/certmagic"

type Options struct {
	TLS            config.TLSConfig
	S3             config.S3Config
	PublicHostname string
}

func TLSConfig(ctx context.Context, opts Options) (*tls.Config, error) {
	switch strings.ToLower(strings.TrimSpace(opts.TLS.Mode)) {
	case "", "manual":
		return manualTLSConfig(opts.TLS)
	case "acme":
		return acmeTLSConfig(ctx, opts)
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

func acmeTLSConfig(ctx context.Context, opts Options) (*tls.Config, error) {
	if opts.PublicHostname == "" {
		return nil, fmt.Errorf("public hostname is required for ACME TLS")
	}
	storage, err := acmeStorage(opts)
	if err != nil {
		return nil, err
	}
	magic := certmagic.NewDefault()
	magic.Storage = storage
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
		DNS01Solver:               dns01Solver(opts.TLS),
	})}
	if err := magic.ManageAsync(ctx, []string{opts.PublicHostname}); err != nil {
		return nil, err
	}
	tlsConfig := magic.TLSConfig()
	tlsConfig.ServerName = opts.PublicHostname
	return tlsConfig, nil
}

func dns01Solver(cfg config.TLSConfig) *certmagic.DNS01Solver {
	switch strings.ToLower(strings.TrimSpace(cfg.ACMEDNSProvider)) {
	case "cloudflare":
		return &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider:      &cloudflare.Provider{APIToken: cfg.ACMECloudflareAPIToken},
			PropagationDelay: 10 * time.Second,
		}}
	default:
		return nil
	}
}

func acmeStorage(opts Options) (certmagic.Storage, error) {
	switch strings.ToLower(strings.TrimSpace(opts.TLS.ACMEStorage)) {
	case "", "file":
		return &certmagic.FileStorage{Path: acmeCachePath(opts.TLS)}, nil
	case "s3":
		return NewS3Storage(opts.S3, opts.TLS.ACMES3Prefix)
	default:
		return nil, fmt.Errorf("unsupported acme storage %q", opts.TLS.ACMEStorage)
	}
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

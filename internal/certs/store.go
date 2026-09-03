package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

type Source interface {
	Load(ctx context.Context, key string) ([]byte, error)
}

type Store struct {
	source    Source
	hostnames []string
	issuerKey string
	mu        sync.RWMutex
	certs     map[string]*tls.Certificate
}

func NewStore(source Source, hostnames []string, issuerKey string) *Store {
	return &Store{source: source, hostnames: hostnames, issuerKey: issuerKey, certs: map[string]*tls.Certificate{}}
}

func (s *Store) Refresh(ctx context.Context) error {
	loaded := map[string]*tls.Certificate{}
	var problems []error
	for _, hostname := range s.hostnames {
		cert, err := s.load(ctx, hostname)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", hostname, err))
			continue
		}
		loaded[hostname] = cert
	}
	if len(loaded) == 0 {
		return errors.Join(problems...)
	}
	s.mu.Lock()
	for hostname, cert := range loaded {
		s.certs[hostname] = cert
	}
	s.mu.Unlock()
	for _, problem := range problems {
		slog.Warn("certificate not loaded from storage", "error", problem)
	}
	return nil
}

func (s *Store) RefreshEvery(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Refresh(ctx); err != nil {
					slog.Error("certificate refresh failed", "error", err)
				}
			}
		}
	}()
}

func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cert, ok := s.certs[strings.ToLower(hello.ServerName)]; ok {
		return cert, nil
	}
	for _, hostname := range s.hostnames {
		if cert, ok := s.certs[hostname]; ok {
			return cert, nil
		}
	}
	return nil, errors.New("no certificate available")
}

func (s *Store) load(ctx context.Context, hostname string) (*tls.Certificate, error) {
	certPEM, err := s.source.Load(ctx, certmagic.StorageKeys.SiteCert(s.issuerKey, hostname))
	if err != nil {
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	keyPEM, err := s.source.Load(ctx, certmagic.StorageKeys.SitePrivateKey(s.issuerKey, hostname))
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse key pair: %w", err)
	}
	return &cert, nil
}

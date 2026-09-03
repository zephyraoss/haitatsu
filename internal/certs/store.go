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

const missingRetryInterval = 30 * time.Second

func (s *Store) Refresh(ctx context.Context) error {
	var problems []error
	for _, hostname := range s.hostnames {
		cert, err := s.load(ctx, hostname)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", hostname, err))
			continue
		}
		s.mu.Lock()
		_, hadBefore := s.certs[hostname]
		s.certs[hostname] = cert
		s.mu.Unlock()
		if !hadBefore {
			slog.Info("certificate loaded from storage", "hostname", hostname)
		}
	}
	return errors.Join(problems...)
}

func (s *Store) Complete() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, hostname := range s.hostnames {
		if _, ok := s.certs[hostname]; !ok {
			return false
		}
	}
	return true
}

func (s *Store) RefreshEvery(ctx context.Context, interval time.Duration) {
	go func() {
		for {
			wait := interval
			if !s.Complete() {
				wait = min(interval, missingRetryInterval)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if err := s.Refresh(ctx); err != nil {
				slog.Warn("certificate refresh incomplete", "error", err)
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
	return nil, errors.New("no certificate available yet")
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

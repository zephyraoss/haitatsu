package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io/fs"
	"math/big"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

type mapSource map[string][]byte

func (m mapSource) Load(_ context.Context, key string) ([]byte, error) {
	data, ok := m[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func selfSigned(t *testing.T, hostname string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func put(source mapSource, issuer, hostname string, certPEM, keyPEM []byte) {
	source[certmagic.StorageKeys.SiteCert(issuer, hostname)] = certPEM
	source[certmagic.StorageKeys.SitePrivateKey(issuer, hostname)] = keyPEM
}

func TestStoreServesCertificateBySNIAndFallsBackToFirstHostname(t *testing.T) {
	const issuer = "acme-v02.api.letsencrypt.org-directory"
	source := mapSource{}
	primaryCert, primaryKey := selfSigned(t, "haitatsu.example.test")
	aliasCert, aliasKey := selfSigned(t, "mx.example.test")
	put(source, issuer, "haitatsu.example.test", primaryCert, primaryKey)
	put(source, issuer, "mx.example.test", aliasCert, aliasKey)

	store := NewStore(source, []string{"haitatsu.example.test", "mx.example.test"}, issuer)
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	cert, err := store.GetCertificate(&tls.ClientHelloInfo{ServerName: "MX.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if leaf(t, cert).DNSNames[0] != "mx.example.test" {
		t.Fatalf("SNI lookup returned certificate for %v", leaf(t, cert).DNSNames)
	}

	cert, err = store.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if leaf(t, cert).DNSNames[0] != "haitatsu.example.test" {
		t.Fatalf("no-SNI lookup returned certificate for %v", leaf(t, cert).DNSNames)
	}
}

func TestStoreServesWhatItHasAndReportsWhatIsMissing(t *testing.T) {
	const issuer = "acme-v02.api.letsencrypt.org-directory"
	source := mapSource{}
	certPEM, keyPEM := selfSigned(t, "haitatsu.example.test")
	put(source, issuer, "haitatsu.example.test", certPEM, keyPEM)

	store := NewStore(source, []string{"haitatsu.example.test", "mx.example.test"}, issuer)
	if err := store.Refresh(context.Background()); err == nil {
		t.Fatal("refresh should report the missing hostname")
	}
	if store.Complete() {
		t.Fatal("store should not be complete while a hostname is missing")
	}
	if _, err := store.GetCertificate(&tls.ClientHelloInfo{ServerName: "mx.example.test"}); err != nil {
		t.Fatalf("missing hostname should fall back to the primary certificate: %v", err)
	}
}

func TestStorePicksUpCertificatesThatAppearLater(t *testing.T) {
	const issuer = "acme-v02.api.letsencrypt.org-directory"
	source := mapSource{}
	store := NewStore(source, []string{"haitatsu.example.test"}, issuer)
	if err := store.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error while the bucket is empty")
	}
	if _, err := store.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("handshake should fail while no certificate is loaded")
	}

	certPEM, keyPEM := selfSigned(t, "haitatsu.example.test")
	put(source, issuer, "haitatsu.example.test", certPEM, keyPEM)
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.Complete() {
		t.Fatal("store should be complete once the certificate is present")
	}
	if _, err := store.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("certificate should be served after it appears: %v", err)
	}
}

func leaf(t *testing.T, cert *tls.Certificate) *x509.Certificate {
	t.Helper()
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

package submission

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/smtp"
	"strings"
	"testing"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/outbound"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

const password = "app-password"

type harness struct {
	client *ent.Client
	store  *mailstore.Store
	mbox   *ent.Mailbox
	addr   string
}

func newHarness(t *testing.T, limits outbound.Limits) *harness {
	t.Helper()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	blobs := testutil.NewFakeStore()
	mbox := testutil.SeedMailbox(t, store, "carol@example.test")
	hash, _ := passwordauth.HashPassword(password)
	if _, err := client.AppPassword.Create().SetMailboxID(mbox.ID).SetName("t").SetHash(hash).SetScopes([]string{"smtp"}).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := client.DKIMKey.Create().SetDomain("example.test").SetSelector("s").SetPrivateKeyPem(string(private)).SetPublicKeyPem("").Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	submission := outbound.NewSubmission(client, blobs, store, "mail.example.test", "node", func() outbound.Limits { return limits })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(config.SubmissionConfig{StartTLSAddr: listener.Addr().String(), TLSAddr: "127.0.0.1:0"}, "mail.example.test", nil, client, submission, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 10, MaxConnectionsPerIP: 5})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return &harness{client: client, store: store, mbox: mbox, addr: listener.Addr().String()}
}

func submit(addr string, user string, pass string, from string, to []string, body string) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("client.example.test"); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(addr)
	if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(body)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func TestSubmissionQueuesSignedMessage(t *testing.T) {
	h := newHarness(t, outbound.Limits{})
	body := "From: carol@example.test\r\nTo: dave@remote.test\r\nSubject: hi\r\n\r\nhello\r\n"
	if err := submit(h.addr, "carol@example.test", password, "carol@example.test", []string{"dave@remote.test"}, body); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.client.OutboundJob.Query().All(context.Background())
	if len(jobs) != 1 || jobs[0].Status != "queued" || !strings.HasPrefix(jobs[0].ReturnPath, "bounces+") {
		t.Fatalf("jobs = %+v", jobs)
	}
	sent, _ := h.store.FolderByName(context.Background(), h.mbox.ID, "Sent")
	if items, _ := h.store.ActiveMessagesInFolder(context.Background(), sent.ID); len(items) != 1 {
		t.Fatal("sent copy missing")
	}
}

func TestSubmissionRejectsForeignSenderAndBadAuth(t *testing.T) {
	h := newHarness(t, outbound.Limits{})
	body := "Subject: hi\r\n\r\nhello\r\n"
	err := submit(h.addr, "carol@example.test", password, "mallory@example.test", []string{"dave@remote.test"}, body)
	if err == nil || !strings.Contains(err.Error(), "553") {
		t.Fatalf("foreign sender should be rejected with 553, got %v", err)
	}
	err = submit(h.addr, "carol@example.test", "nope", "carol@example.test", []string{"dave@remote.test"}, body)
	if err == nil {
		t.Fatal("bad password should fail auth")
	}
}

func TestSubmissionEnforcesOutboundLimits(t *testing.T) {
	h := newHarness(t, outbound.Limits{PerHour: 1})
	body := "From: carol@example.test\r\nTo: dave@remote.test\r\nSubject: hi\r\n\r\nhello\r\n"
	if err := submit(h.addr, "carol@example.test", password, "carol@example.test", []string{"dave@remote.test"}, body); err != nil {
		t.Fatal(err)
	}
	err := submit(h.addr, "carol@example.test", password, "carol@example.test", []string{"dave@remote.test"}, body)
	if err == nil || !strings.Contains(err.Error(), "450") {
		t.Fatalf("rate limited submission should return 450, got %v", err)
	}
}

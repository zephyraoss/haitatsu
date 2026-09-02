package outbound

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func seedDKIM(t *testing.T, client *ent.Client, domain string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	public, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if _, err := client.DKIMKey.Create().SetDomain(domain).SetSelector("s1").SetPrivateKeyPem(string(private)).SetPublicKeyPem(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newSubmission(t *testing.T, defaults Limits) (*Submission, *ent.Mailbox, *testutil.FakeStore) {
	t.Helper()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "alice@example.test")
	seedDKIM(t, client, "example.test")
	blobs := testutil.NewFakeStore()
	return NewSubmission(client, blobs, store, "mail.example.test", "node-1", func() Limits { return defaults }), mbox, blobs
}

func TestSubmitEnforcesRecipientLimit(t *testing.T) {
	submission, mbox, _ := newSubmission(t, Limits{RecipientsPerMessage: 2})
	raw := []byte("From: alice@example.test\r\nTo: a@x.test, b@x.test, c@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	_, err := submission.Submit(context.Background(), mbox.ID, "alice@example.test", raw, nil)
	if !errors.Is(err, ErrTooManyRecipients) {
		t.Fatalf("expected ErrTooManyRecipients, got %v", err)
	}
}

func TestSubmitEnforcesHourlyLimit(t *testing.T) {
	submission, mbox, _ := newSubmission(t, Limits{PerHour: 1})
	raw := []byte("From: alice@example.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	if _, err := submission.Submit(context.Background(), mbox.ID, "alice@example.test", raw, nil); err != nil {
		t.Fatal(err)
	}
	_, err := submission.Submit(context.Background(), mbox.ID, "alice@example.test", raw, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestSubmitMailboxOverrideBeatsDefaults(t *testing.T) {
	submission, mbox, _ := newSubmission(t, Limits{PerHour: 1})
	if _, err := submission.client.Mailbox.UpdateOneID(mbox.ID).SetOutboundLimits(map[string]int64{"per_hour": 5}).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: alice@example.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	for range 3 {
		if _, err := submission.Submit(context.Background(), mbox.ID, "alice@example.test", raw, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubmitStripsBccAndKeepsBccRecipients(t *testing.T) {
	submission, mbox, blobs := newSubmission(t, Limits{})
	raw := []byte("From: alice@example.test\r\nTo: a@x.test\r\nBcc: hidden@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	msg, err := submission.Submit(context.Background(), mbox.ID, "alice@example.test", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := blobs.Objects[msg.BlobKey]
	header, _ := mailparse.SplitHeaderBody(stored)
	if strings.Contains(strings.ToLower(string(header)), "bcc:") {
		t.Fatalf("Bcc header leaked into relayed message: %q", header)
	}
	if !strings.Contains(string(header), "DKIM-Signature:") {
		t.Fatalf("message was not DKIM signed: %q", header)
	}
	job, err := submission.client.OutboundJob.Query().First(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Recipients) != 2 {
		t.Fatalf("recipients = %v, want To and Bcc envelope recipients", job.Recipients)
	}
	sent, _ := submission.store.FolderByName(context.Background(), mbox.ID, "Sent")
	items, _ := submission.store.ActiveMessagesInFolder(context.Background(), sent.ID)
	if len(items) != 1 || !items[0].Read || items[0].UID != 1 {
		t.Fatalf("sent copy = %+v", items)
	}
	reloaded, _ := submission.client.Mailbox.Get(context.Background(), mbox.ID)
	if reloaded.UsedBytes != int64(len(stored)) {
		t.Fatalf("used_bytes = %d, want %d", reloaded.UsedBytes, len(stored))
	}
}

func TestSenderAllowedViaRoute(t *testing.T) {
	submission, mbox, _ := newSubmission(t, Limits{})
	ctx := context.Background()
	if _, err := submission.client.Route.Create().SetSourceAddress("sales@example.test").SetType("alias").SetDestinations([]string{mbox.ID}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	allowed, err := submission.SenderAllowed(ctx, mbox, "Sales@Example.test")
	if err != nil || !allowed {
		t.Fatalf("route alias should be allowed: %v %v", allowed, err)
	}
	allowed, _ = submission.SenderAllowed(ctx, mbox, "other@example.test")
	if allowed {
		t.Fatal("unrelated address must not be allowed")
	}
	if _, err := submission.client.Mailbox.UpdateOneID(mbox.ID).SetQuotaBytes(10).Save(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = submission.Submit(ctx, mbox.ID, "alice@example.test", []byte("From: alice@example.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n"), nil)
	if !errors.Is(err, mailstore.ErrOverQuota) {
		t.Fatalf("expected over quota, got %v", err)
	}
}

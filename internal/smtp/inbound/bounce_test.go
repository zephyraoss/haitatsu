package inbound

import (
	"context"
	"net/smtp"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func sendMany(t *testing.T, addr string, recipients []string, body string) []error {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("mailer-daemon@remote.test"); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, len(recipients))
	for i, rcpt := range recipients {
		errs[i] = c.Rcpt(rcpt)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return errs
}

func seedHostedDomain(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.client.DKIMKey.Create().SetDomain("example.test").SetSelector("s1").SetPrivateKeyPem("private").SetPublicKeyPem("public").Save(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func bounceObjects(h *harness) int {
	count := 0
	for key := range h.blobs.Objects {
		if strings.HasPrefix(key, "bounces/") {
			count++
		}
	}
	return count
}

func TestInboundBounceStoresPayloadOnceAndCapsRecipients(t *testing.T) {
	h := newHarness(t, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 100, MaxConnectionsPerIP: 5, MessagesPerMinute: 1000})
	seedHostedDomain(t, h)
	duplicate := "bounces+" + strings.ToLower(ulid.Make().String()) + "@example.test"
	recipients := []string{duplicate, duplicate}
	for range maxBounceRecipients + 5 {
		recipients = append(recipients, "bounces+"+strings.ToLower(ulid.Make().String())+"@example.test")
	}
	errs := sendMany(t, h.addr, recipients, "From: mailer-daemon@remote.test\r\nSubject: failure\r\n\r\nundeliverable\r\n")
	rejected := 0
	for _, err := range errs {
		if err != nil {
			if !strings.Contains(err.Error(), "452") {
				t.Fatalf("unexpected rcpt error: %v", err)
			}
			rejected++
		}
	}
	if rejected != len(recipients)-1-maxBounceRecipients {
		t.Fatalf("rejected = %d", rejected)
	}
	events, err := h.client.BounceEvent.Query().All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != maxBounceRecipients {
		t.Fatalf("bounce events = %d, want %d", len(events), maxBounceRecipients)
	}
	for _, event := range events[1:] {
		if event.BlobKey != events[0].BlobKey {
			t.Fatalf("bounce events should share one blob, got %q and %q", event.BlobKey, events[0].BlobKey)
		}
	}
	if got := bounceObjects(h); got != 1 {
		t.Fatalf("bounce blobs = %d, want 1", got)
	}
}

func TestInboundChargesRateLimitPerRecipient(t *testing.T) {
	h := newHarness(t, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 100, MaxConnectionsPerIP: 5, MessagesPerMinute: 3})
	seedHostedDomain(t, h)
	var recipients []string
	for range 5 {
		recipients = append(recipients, "bounces+"+strings.ToLower(ulid.Make().String())+"@example.test")
	}
	errs := sendMany(t, h.addr, recipients, "From: mailer-daemon@remote.test\r\nSubject: failure\r\n\r\nundeliverable\r\n")
	for i, err := range errs {
		if i < 3 && err != nil {
			t.Fatalf("recipient %d should be accepted, got %v", i, err)
		}
		if i >= 3 && (err == nil || !strings.Contains(err.Error(), "450")) {
			t.Fatalf("recipient %d should be rate limited with 450, got %v", i, err)
		}
	}
	if count, _ := h.client.BounceEvent.Query().Count(context.Background()); count != 3 {
		t.Fatalf("bounce events = %d, want 3", count)
	}
}

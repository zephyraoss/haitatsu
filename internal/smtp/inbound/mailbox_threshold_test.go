package inbound

import (
	"context"
	"net/smtp"
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestInboundMailboxThresholdsAndReset(t *testing.T) {
	h := newHarness(t, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 10, MaxConnectionsPerIP: 5, MessagesPerMinute: 60})
	ctx := context.Background()
	alice := testutil.SeedMailbox(t, h.store, "alice@example.test")
	if _, err := h.client.Mailbox.UpdateOneID(alice.ID).SetSpamThresholds(map[string]float64{"junk_threshold": 0}).Save(ctx); err != nil {
		t.Fatal(err)
	}

	sendBoth := func() {
		t.Helper()
		client, err := smtp.Dial(h.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if err := client.Hello("client.example.test"); err != nil {
			t.Fatal(err)
		}
		if err := client.Mail("sender@remote.test"); err != nil {
			t.Fatal(err)
		}
		for _, recipient := range []string{"alice+news@example.test", h.mbox.PrimaryAddress} {
			if err := client.Rcpt(recipient); err != nil {
				t.Fatal(err)
			}
		}
		writer, err := client.Data()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("Subject: shared delivery\r\nMessage-ID: <threshold@local>\r\n\r\nHello both.\r\n")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := client.Quit(); err != nil {
			t.Fatal(err)
		}
	}
	assertFolder := func(mailboxID, name string, count int) string {
		t.Helper()
		folder, err := h.store.FolderByName(ctx, mailboxID, name)
		if err != nil {
			t.Fatal(err)
		}
		items, err := h.store.ActiveMessagesInFolder(ctx, folder.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != count {
			t.Fatalf("mailbox %s folder %s: got %d messages, want %d", mailboxID, name, len(items), count)
		}
		if count == 1 {
			return items[0].MessageID
		}
		return ""
	}

	sendBoth()
	aliceMessage := assertFolder(alice.ID, "Junk", 1)
	bobMessage := assertFolder(h.mbox.ID, "INBOX", 1)
	if aliceMessage != bobMessage {
		t.Fatal("recipients should share one stored message")
	}
	assertFolder(alice.ID, "INBOX", 0)
	assertFolder(h.mbox.ID, "Junk", 0)

	if _, err := h.client.Mailbox.UpdateOneID(alice.ID).ClearSpamThresholds().Save(ctx); err != nil {
		t.Fatal(err)
	}
	sendBoth()
	assertFolder(alice.ID, "INBOX", 1)
	assertFolder(alice.ID, "Junk", 1)
	assertFolder(h.mbox.ID, "INBOX", 2)
}

func TestMailboxThresholdDoesNotBypassSMTPRejection(t *testing.T) {
	h := newHarness(t, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 10, MaxConnectionsPerIP: 5, MessagesPerMinute: 60})
	ctx := context.Background()
	if _, err := h.client.Mailbox.UpdateOneID(h.mbox.ID).SetSpamThresholds(map[string]float64{"junk_threshold": 100}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	// A sender block contributes 10 points, reaching the global reject threshold.
	if _, err := h.client.SenderRule.Create().SetScope("global").SetKind("block").SetMatchType("ip").SetValue("127.0.0.1").Save(ctx); err != nil {
		t.Fatal(err)
	}
	body := "Subject: blocked sender\r\nMessage-ID: <blocked@local>\r\n\r\nBlocked message.\r\n"
	if err := send(h.addr, "sender@remote.test", h.mbox.PrimaryAddress, body); err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("expected global SMTP rejection, got %v", err)
	}
	count, err := h.client.Message.Query().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected message was stored: count=%d", count)
	}
}

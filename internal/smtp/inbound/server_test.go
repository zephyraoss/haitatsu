package inbound

import (
	"context"
	"net"
	"net/smtp"
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/bounce"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/rules"
	"github.com/zephyraoss/haitatsu/internal/spam"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type harness struct {
	client *ent.Client
	store  *mailstore.Store
	mbox   *ent.Mailbox
	addr   string
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	blobs := testutil.NewFakeStore()
	mbox := testutil.SeedMailbox(t, store, "bob@example.test")
	m := metrics.New()
	eventService := events.New(client)
	engine := rules.New(client, store, eventService)
	service := messages.NewService(client, blobs, store, eventService, engine, m, "mx.example.test", "node")
	checker := spam.NewChecker(client, func() config.SpamConfig { return config.SpamConfig{} }, "mx.example.test")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(config.SMTPConfig{InboundAddr: listener.Addr().String()}, "mx.example.test", nil, routing.NewResolver(client), service, bounce.NewHandler(client, blobs, m), checker, m, opts)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return &harness{client: client, store: store, mbox: mbox, addr: listener.Addr().String()}
}

func send(addr string, from string, to string, body string) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("client.example.test"); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
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

func TestInboundDeliversToInbox(t *testing.T) {
	h := newHarness(t, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 10, MaxConnectionsPerIP: 5, MessagesPerMinute: 60})
	body := "From: sender@remote.test\r\nTo: bob@example.test\r\nSubject: hello\r\nMessage-ID: <1@remote.test>\r\n\r\nhi\r\n"
	if err := send(h.addr, "sender@remote.test", "bob+news@example.test", body); err != nil {
		t.Fatal(err)
	}
	inbox, _ := h.store.FolderByName(context.Background(), h.mbox.ID, "INBOX")
	items, _ := h.store.ActiveMessagesInFolder(context.Background(), inbox.ID)
	if len(items) != 1 || items[0].UID != 1 || items[0].PlusTag != "news" {
		t.Fatalf("delivered = %+v", items)
	}
	reloaded, _ := h.client.Mailbox.Get(context.Background(), h.mbox.ID)
	if reloaded.UsedBytes == 0 {
		t.Fatal("usage should be recorded")
	}
	if err := send(h.addr, "sender@remote.test", "nobody@example.test", body); err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("unknown recipient should be rejected with 550, got %v", err)
	}
}

func TestInboundRejectsOverQuotaAndRateLimits(t *testing.T) {
	h := newHarness(t, Options{MaxMessageBytes: 1 << 20, MaxRecipients: 10, MaxConnectionsPerIP: 5, MessagesPerMinute: 1})
	if _, err := h.client.Mailbox.UpdateOneID(h.mbox.ID).SetQuotaBytes(1).SetUsedBytes(1).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := "From: sender@remote.test\r\nTo: bob@example.test\r\nSubject: hello\r\n\r\nhi\r\n"
	err := send(h.addr, "sender@remote.test", "bob@example.test", body)
	if err == nil || !strings.Contains(err.Error(), "452") {
		t.Fatalf("over quota should return 452, got %v", err)
	}
	err = send(h.addr, "sender@remote.test", "bob@example.test", body)
	if err == nil || !strings.Contains(err.Error(), "450") {
		t.Fatalf("second message within the minute should be rate limited with 450, got %v", err)
	}
}

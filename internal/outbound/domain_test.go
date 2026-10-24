package outbound

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

const transactionalMessage = "From: receipts@example.test\r\nTo: a@x.test\r\nBcc: audit@x.test\r\nSubject: receipt\r\n\r\nthanks\r\n"

func TestSubmitForDomainQueuesSignedJobWithoutMailboxCopy(t *testing.T) {
	submission, mbox, blobs := newSubmission(t, Limits{})
	ctx := context.Background()
	msg, err := submission.SubmitForDomain(ctx, "Example.test", "receipts@example.test", []byte(transactionalMessage), nil)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := mailparse.SplitHeaderBody(blobs.Objects[msg.BlobKey])
	if !strings.Contains(string(header), "DKIM-Signature:") {
		t.Fatalf("message was not DKIM signed: %q", header)
	}
	if strings.Contains(strings.ToLower(string(header)), "bcc:") {
		t.Fatalf("Bcc header leaked into relayed message: %q", header)
	}
	job, err := submission.client.OutboundJob.Query().Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job.MailboxID != "" || job.SenderDomain != "example.test" || job.ReturnPath != ReturnPath(msg.ID, "example.test") || len(job.Recipients) != 2 {
		t.Fatalf("unexpected job %+v", job)
	}
	copies, err := submission.client.MailboxMessage.Query().Where(mailboxmessage.MessageIDEQ(msg.ID)).Count(ctx)
	if err != nil || copies != 0 {
		t.Fatalf("domain send stored %d mailbox copies (%v)", copies, err)
	}
	reloaded, _ := submission.client.Mailbox.Get(ctx, mbox.ID)
	if reloaded.UsedBytes != 0 {
		t.Fatalf("domain send charged mailbox quota: used_bytes = %d", reloaded.UsedBytes)
	}
}

func TestSubmitForDomainRejectsSenderOutsideDomain(t *testing.T) {
	submission, _, _ := newSubmission(t, Limits{})
	ctx := context.Background()
	_, err := submission.SubmitForDomain(ctx, "example.test", "receipts@other.test", []byte(transactionalMessage), nil)
	if !errors.Is(err, ErrSenderNotAllowed) {
		t.Fatalf("envelope sender outside domain: got %v", err)
	}
	spoofed := "From: ceo@other.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n"
	_, err = submission.SubmitForDomain(ctx, "example.test", "receipts@example.test", []byte(spoofed), nil)
	if !errors.Is(err, ErrSenderNotAllowed) {
		t.Fatalf("header From outside domain: got %v", err)
	}
}

func TestSubmitForDomainRequiresDKIMKey(t *testing.T) {
	submission, _, _ := newSubmission(t, Limits{})
	raw := "From: hi@unsigned.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n"
	_, err := submission.SubmitForDomain(context.Background(), "unsigned.test", "hi@unsigned.test", []byte(raw), nil)
	if !errors.Is(err, ErrDKIMKeyNotFound) {
		t.Fatalf("expected ErrDKIMKeyNotFound, got %v", err)
	}
}

func TestSubmitForDomainUsesDomainLimitsSeparatelyFromMailboxes(t *testing.T) {
	submission, mbox, _ := newSubmission(t, Limits{PerHour: 1})
	submission.WithDomainLimits(func() Limits { return Limits{PerHour: 3} })
	ctx := context.Background()
	mailboxRaw := []byte("From: alice@example.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	if _, err := submission.Submit(ctx, mbox.ID, "alice@example.test", mailboxRaw, nil); err != nil {
		t.Fatal(err)
	}
	domainRaw := []byte("From: receipts@example.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	for range 3 {
		if _, err := submission.SubmitForDomain(ctx, "example.test", "receipts@example.test", domainRaw, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := submission.SubmitForDomain(ctx, "example.test", "receipts@example.test", domainRaw, nil); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("fourth domain send: expected ErrRateLimited, got %v", err)
	}
	if _, err := submission.Submit(ctx, mbox.ID, "alice@example.test", mailboxRaw, nil); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("domain sends must not refill the mailbox budget: got %v", err)
	}
}

func TestSubmitForDomainConcurrentRespectsHourlyLimit(t *testing.T) {
	submission, _, _ := newSubmission(t, Limits{})
	submission.WithDomainLimits(func() Limits { return Limits{PerHour: 2} })
	raw := []byte("From: receipts@example.test\r\nTo: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for range 6 {
		wg.Go(func() {
			_, err := submission.SubmitForDomain(context.Background(), "example.test", "receipts@example.test", raw, nil)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrRateLimited) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 2 {
		t.Fatalf("accepted %d submissions, want 2", accepted.Load())
	}
}

type recordedEvent struct {
	eventType string
	mailboxID string
	payload   map[string]any
}

type recordingEvents struct{ events []recordedEvent }

func (r *recordingEvents) Emit(_ context.Context, eventType string, mailboxID string, payload map[string]any) error {
	r.events = append(r.events, recordedEvent{eventType: eventType, mailboxID: mailboxID, payload: payload})
	return nil
}

func TestClaimReturnsSenderDomain(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewClient(t)
	if _, err := client.OutboundJob.Create().SetMailboxID("").SetSenderDomain("example.test").SetMessageID("msg").SetReturnPath("bounces+msg@example.test").SetRecipients([]string{"a@x.test"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(db, client, testutil.NewFakeStore(), func() config.RelayConfig { return config.RelayConfig{} }, metrics.New(), nil, "w1", database.BackendSQLite)
	job, ok, err := worker.claim(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if job.SenderDomain != "example.test" || job.MailboxID != "" {
		t.Fatalf("claimed = %+v", job)
	}
}

func TestFinishEmitsBounceEventForDomainJob(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	notifier := &recordingNotifier{}
	emitter := &recordingEvents{}
	worker := NewWorker(nil, client, testutil.NewFakeStore(), func() config.RelayConfig { return config.RelayConfig{Addr: "relay:25"} }, metrics.New(), notifier, "w1", database.BackendSQLite).WithEvents(emitter)
	job, err := client.OutboundJob.Create().SetMailboxID("").SetSenderDomain("example.test").SetMessageID("msg").SetReturnPath("bounces+msg@example.test").SetRecipients([]string{"a@x.test"}).SetLockedBy("w1").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimedJob{ID: job.ID, SenderDomain: "example.test", MessageID: "msg", Recipients: []string{"a@x.test"}}
	if err := worker.finish(ctx, claimed, permanentFailure(550, "5.1.1", "no such user")); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 0 {
		t.Fatal("domain job failures must not deliver a mailbox notification")
	}
	if len(emitter.events) != 1 {
		t.Fatalf("events = %+v", emitter.events)
	}
	event := emitter.events[0]
	if event.eventType != events.MessageBounced || event.mailboxID != "" || event.payload["source"] != "relay" || event.payload["sender_domain"] != "example.test" || event.payload["message_id"] != "msg" {
		t.Fatalf("unexpected event %+v", event)
	}
}

func TestFinishNotifiesMailboxForMailboxJob(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	notifier := &recordingNotifier{}
	emitter := &recordingEvents{}
	worker := NewWorker(nil, client, testutil.NewFakeStore(), func() config.RelayConfig { return config.RelayConfig{Addr: "relay:25"} }, metrics.New(), notifier, "w1", database.BackendSQLite).WithEvents(emitter)
	job, err := client.OutboundJob.Create().SetMailboxID("m").SetMessageID("msg").SetReturnPath("bounces+msg@example.test").SetRecipients([]string{"a@x.test"}).SetLockedBy("w1").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.finish(ctx, claimedJob{ID: job.ID, MailboxID: "m", MessageID: "msg"}, permanentFailure(550, "5.1.1", "no such user")); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 || len(emitter.events) != 0 {
		t.Fatalf("notifier calls = %d, events = %+v", notifier.calls, emitter.events)
	}
}

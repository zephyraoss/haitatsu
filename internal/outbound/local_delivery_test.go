package outbound

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type localHarness struct {
	submission *Submission
	blobs      *testutil.FakeStore
	alice      *ent.Mailbox
	bob        *ent.Mailbox
}

func newLocalHarness(t *testing.T, client *ent.Client) localHarness {
	t.Helper()
	store := testutil.NewMailStore(t, client)
	alice := testutil.SeedMailbox(t, store, "alice@example.test")
	bob := testutil.SeedMailbox(t, store, "bob@other.test")
	seedDKIM(t, client, "example.test")
	blobs := testutil.NewFakeStore()
	deliverer := messages.NewService(client, blobs, store, nil, nil, metrics.New(), "mail.example.test", "node-1")
	submission := NewSubmission(client, blobs, store, "mail.example.test", "node-1", func() Limits { return Limits{} }).
		WithLocalDelivery(routing.NewResolver(client), deliverer)
	return localHarness{submission: submission, blobs: blobs, alice: alice, bob: bob}
}

func (h localHarness) folderBlobs(t *testing.T, mailboxID string, name string) []string {
	t.Helper()
	ctx := context.Background()
	f, err := h.submission.store.FolderByName(ctx, mailboxID, name)
	if err != nil {
		t.Fatal(err)
	}
	items, err := h.submission.store.ActiveMessagesInFolder(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var raws []string
	for _, item := range items {
		msg, err := h.submission.client.Message.Get(ctx, item.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, string(h.blobs.Objects[msg.BlobKey]))
	}
	return raws
}

func (h localHarness) relayJobs(t *testing.T) []*ent.OutboundJob {
	t.Helper()
	jobs, err := h.submission.client.OutboundJob.Query().All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestSubmitDeliversLocalRecipientsWithoutRelay(t *testing.T) {
	client, _ := testutil.NewClient(t)
	assertLocalDelivery(t, newLocalHarness(t, client))
}

func TestSubmitDeliversLocalRecipientsOnPostgres(t *testing.T) {
	assertLocalDelivery(t, newLocalHarness(t, testutil.NewPostgresClient(t)))
}

func assertLocalDelivery(t *testing.T, h localHarness) {
	t.Helper()
	raw := []byte("From: alice@example.test\r\nTo: Bob@Other.test, dave@remote.test\r\nSubject: hi\r\n\r\nbody\r\n")
	if _, err := h.submission.Submit(context.Background(), h.alice.ID, "alice@example.test", raw, nil); err != nil {
		t.Fatal(err)
	}
	inbox := h.folderBlobs(t, h.bob.ID, "INBOX")
	if len(inbox) != 1 {
		t.Fatalf("bob inbox has %d messages, want 1", len(inbox))
	}
	if !strings.Contains(inbox[0], "DKIM-Signature:") || !strings.Contains(inbox[0], "auth=pass smtp.auth=alice@example.test") {
		t.Fatalf("local copy missing signature or auth results: %q", inbox[0])
	}
	if sent := h.folderBlobs(t, h.alice.ID, "Sent"); len(sent) != 1 {
		t.Fatalf("alice sent has %d messages, want 1", len(sent))
	}
	jobs := h.relayJobs(t)
	if len(jobs) != 1 || !slices.Equal(jobs[0].Recipients, []string{"dave@remote.test"}) {
		t.Fatalf("relay jobs = %+v, want only the remote recipient", jobs)
	}
}

func TestSubmitWithOnlyLocalRecipientsQueuesNothing(t *testing.T) {
	client, _ := testutil.NewClient(t)
	h := newLocalHarness(t, client)
	raw := []byte("From: alice@example.test\r\nTo: bob@other.test\r\nSubject: hi\r\n\r\nbody\r\n")
	if _, err := h.submission.Submit(context.Background(), h.alice.ID, "alice@example.test", raw, nil); err != nil {
		t.Fatal(err)
	}
	if jobs := h.relayJobs(t); len(jobs) != 0 {
		t.Fatalf("relay jobs = %+v, want none", jobs)
	}
	if inbox := h.folderBlobs(t, h.bob.ID, "INBOX"); len(inbox) != 1 {
		t.Fatalf("bob inbox has %d messages, want 1", len(inbox))
	}
}

func TestSubmitRejectsLocalRecipientOverQuota(t *testing.T) {
	client, _ := testutil.NewClient(t)
	h := newLocalHarness(t, client)
	if _, err := client.Mailbox.UpdateOneID(h.bob.ID).SetQuotaBytes(10).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: alice@example.test\r\nTo: bob@other.test, dave@remote.test\r\nSubject: hi\r\n\r\nbody\r\n")
	_, err := h.submission.Submit(context.Background(), h.alice.ID, "alice@example.test", raw, nil)
	if !errors.Is(err, ErrRecipientOverQuota) {
		t.Fatalf("expected ErrRecipientOverQuota, got %v", err)
	}
	if sent := h.folderBlobs(t, h.alice.ID, "Sent"); len(sent) != 0 {
		t.Fatalf("rejected submission left %d sent copies", len(sent))
	}
	if jobs := h.relayJobs(t); len(jobs) != 0 {
		t.Fatalf("rejected submission queued %+v", jobs)
	}
}

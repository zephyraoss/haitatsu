package imapserver_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/imapserver"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

const testPassword = "correct-horse-battery"

type harness struct {
	t      *testing.T
	client *ent.Client
	store  *mailstore.Store
	blobs  *testutil.FakeStore
	mbox   *ent.Mailbox
	addr   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	blobs := testutil.NewFakeStore()
	mbox := testutil.SeedMailbox(t, store, "alice@example.test")
	hash, err := passwordauth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppPassword.Create().SetMailboxID(mbox.ID).SetName("test").SetHash(hash).SetScopes([]string{"imap"}).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := imapserver.New(config.IMAPConfig{Addr: listener.Addr().String()}, nil, client, blobs, store, metrics.New(), imapserver.Options{MaxConnectionsPerIP: 10, AppendLimit: 1 << 20})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return &harness{t: t, client: client, store: store, blobs: blobs, mbox: mbox, addr: listener.Addr().String()}
}

func (h *harness) deliver(folderName string, subject string, flags mailstore.Flags) *ent.MailboxMessage {
	h.t.Helper()
	ctx := context.Background()
	raw := testutil.RawMessage(subject)
	metadata := mailparse.Parse(raw)
	key := "blob-" + ids.New().String()
	_ = h.blobs.PutMessage(ctx, key, raw)
	msg, err := h.client.Message.Create().SetID(ids.New().String()).SetTraceID("t").SetBlobKey(key).SetSha256(ids.New().String()).SetSizeBytes(int64(len(raw))).SetSubject(metadata.Subject).SetFromAddresses(metadata.From).SetToAddresses(metadata.To).SetHeaders(metadata.Headers).SetRfcMessageID(metadata.RFCMessageID).SetDate(*metadata.Date).Save(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	folder, err := h.store.FolderByName(ctx, h.mbox.ID, folderName)
	if err != nil {
		h.t.Fatal(err)
	}
	item, err := h.store.Attach(ctx, mailstore.Attach{MailboxID: h.mbox.ID, MessageID: msg.ID, FolderID: folder.ID, SizeBytes: int64(len(raw)), Flags: flags})
	if err != nil {
		h.t.Fatal(err)
	}
	return item
}

func (h *harness) dial(handler *imapclient.UnilateralDataHandler) *imapclient.Client {
	h.t.Helper()
	client, err := imapclient.DialInsecure(h.addr, &imapclient.Options{UnilateralDataHandler: handler})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = client.Close() })
	if err := client.Login("alice@example.test", testPassword).Wait(); err != nil {
		h.t.Fatal(err)
	}
	return client
}

func TestLoginRejectsBadPassword(t *testing.T) {
	h := newHarness(t)
	client, err := imapclient.DialInsecure(h.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Login("alice@example.test", "wrong").Wait(); err == nil {
		t.Fatal("bad password should fail")
	}
}

func TestUIDsAreStableAcrossExpunge(t *testing.T) {
	h := newHarness(t)
	h.deliver("INBOX", "one", mailstore.Flags{})
	h.deliver("INBOX", "two", mailstore.Flags{})
	h.deliver("INBOX", "three", mailstore.Flags{})
	client := h.dial(nil)

	selected, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if selected.NumMessages != 3 || selected.UIDNext != 4 || selected.UIDValidity == 0 {
		t.Fatalf("select data = %+v", selected)
	}
	if _, err := client.Store(imap.SeqSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	expunged, err := client.Expunge().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(expunged) != 1 || expunged[0] != 2 {
		t.Fatalf("expunged = %v", expunged)
	}
	fetched, err := client.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 2 || fetched[0].UID != 1 || fetched[1].UID != 3 {
		t.Fatalf("uids after expunge = %d,%d (want 1,3)", fetched[0].UID, fetched[1].UID)
	}
	if fetched[1].Envelope.Subject != "three" {
		t.Fatalf("seq 2 should now be subject three, got %q", fetched[1].Envelope.Subject)
	}
	trash, _ := client.Status("Trash", &imap.StatusOptions{NumMessages: true}).Wait()
	if *trash.NumMessages != 1 {
		t.Fatal("expunge from INBOX should move to Trash")
	}
	var uidSet imap.UIDSet
	uidSet.AddNum(3)
	byUID, err := client.Fetch(uidSet, &imap.FetchOptions{Flags: true, RFC822Size: true}).Collect()
	if err != nil || len(byUID) != 1 || byUID[0].SeqNum != 2 || byUID[0].RFC822Size == 0 {
		t.Fatalf("uid fetch = %+v err=%v", byUID, err)
	}
}

func TestFlagsOnlyFetchDoesNotTouchBlobStore(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		h.deliver("INBOX", strings.Repeat("m", i+1), mailstore.Flags{})
	}
	client := h.dial(nil)
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	before := h.blobs.Gets
	var all imap.SeqSet
	all.AddRange(1, 0)
	if _, err := client.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}).Collect(); err != nil {
		t.Fatal(err)
	}
	if h.blobs.Gets != before {
		t.Fatalf("metadata-only fetch hit blob store %d times", h.blobs.Gets-before)
	}
	msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil || len(msgs) != 1 || h.blobs.Gets != before+1 {
		t.Fatalf("body fetch should load exactly one blob: gets=%d err=%v", h.blobs.Gets-before, err)
	}
	if !strings.Contains(string(msgs[0].BodySection[0].Bytes), "body of m") {
		t.Fatalf("unexpected body %q", msgs[0].BodySection[0].Bytes)
	}
}

func TestSearchHonoursCriteria(t *testing.T) {
	h := newHarness(t)
	h.deliver("INBOX", "invoice for widgets", mailstore.Flags{Seen: true})
	h.deliver("INBOX", "lunch plans", mailstore.Flags{})
	h.deliver("INBOX", "invoice reminder", mailstore.Flags{Flagged: true})
	client := h.dial(nil)
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	unseen, err := client.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := unseen.AllUIDs(); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("unseen uids = %v", got)
	}
	subject, err := client.Search(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "invoice"}}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := subject.AllSeqNums(); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("subject seqs = %v", got)
	}
	body, err := client.Search(&imap.SearchCriteria{Body: []string{"lunch"}}, nil).Wait()
	if err != nil || len(body.AllSeqNums()) != 1 || body.AllSeqNums()[0] != 2 {
		t.Fatalf("body search = %v err=%v", body.AllSeqNums(), err)
	}
	flagged, _ := client.Search(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}, Not: []imap.SearchCriteria{{Flag: []imap.Flag{imap.FlagSeen}}}}, nil).Wait()
	if got := flagged.AllSeqNums(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("flagged-not-seen = %v", got)
	}
	since, _ := client.Search(&imap.SearchCriteria{SentSince: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, nil).Wait()
	if len(since.AllSeqNums()) != 0 {
		t.Fatal("future SENTSINCE should match nothing")
	}
}

func TestIdleReceivesNewMailAndFlagChanges(t *testing.T) {
	h := newHarness(t)
	h.deliver("INBOX", "existing", mailstore.Flags{})
	exists := make(chan uint32, 4)
	flagged := make(chan uint32, 4)
	client := h.dial(&imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				exists <- *data.NumMessages
			}
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			for {
				item := msg.Next()
				if item == nil {
					return
				}
				if _, ok := item.(imapclient.FetchItemDataFlags); ok {
					flagged <- msg.SeqNum
				}
			}
		},
	})
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	idle, err := client.Idle()
	if err != nil {
		t.Fatal(err)
	}
	item := h.deliver("INBOX", "arrived while idling", mailstore.Flags{})
	select {
	case n := <-exists:
		if n != 2 {
			t.Fatalf("EXISTS = %d, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no EXISTS during IDLE")
	}
	flags := mailstore.FlagsOf(item)
	flags.Seen = true
	if _, err := h.store.SetFlags(context.Background(), item, flags); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-flagged:
		if seq != 2 {
			t.Fatalf("flag update seq = %d", seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no FETCH FLAGS during IDLE")
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendMoveCopyAndLabels(t *testing.T) {
	h := newHarness(t)
	client := h.dial(nil)
	raw := testutil.RawMessage("appended")
	append := client.Append("Drafts", int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}})
	if _, err := append.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := append.Close(); err != nil {
		t.Fatal(err)
	}
	appended, err := append.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if appended.UID != 1 {
		t.Fatalf("append uid = %d", appended.UID)
	}
	if _, err := client.Select("Drafts", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	fetched, _ := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	flags := fetched[0].Flags
	if !containsFlag(flags, imap.FlagSeen) || !containsFlag(flags, imap.FlagDraft) {
		t.Fatalf("draft flags = %v", flags)
	}
	if err := client.Create("Labels/Important", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	copied, err := client.Copy(imap.SeqSetNum(1), "Labels/Important").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if copied.DestUIDs.String() != "1" {
		t.Fatalf("label copy dest uids = %s", copied.DestUIDs.String())
	}
	moved, err := client.Move(imap.SeqSetNum(1), "Archive").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if moved.DestUIDs.String() != "1" {
		t.Fatalf("move dest uids = %v", moved.DestUIDs)
	}
	drafts, _ := client.Status("Drafts", &imap.StatusOptions{NumMessages: true}).Wait()
	if *drafts.NumMessages != 0 {
		t.Fatal("moved message still in Drafts")
	}
	if _, err := client.Select("Labels/Important", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	labelled, _ := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if len(labelled) != 1 || labelled[0].Envelope.Subject != "appended" {
		t.Fatalf("label view = %+v", labelled)
	}
	reloaded, _ := h.client.Mailbox.Get(context.Background(), h.mbox.ID)
	if reloaded.UsedBytes != int64(len(mailparse.NormalizeMessage(raw))) {
		t.Fatalf("used_bytes = %d, want %d", reloaded.UsedBytes, len(raw))
	}
	listed, err := client.List("", "*", &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]imap.MailboxAttr{}
	for _, item := range listed {
		names[item.Mailbox] = item.Attrs
	}
	if _, ok := names["Labels/Important"]; !ok || !containsAttr(names["Trash"], imap.MailboxAttrTrash) || !containsAttr(names["Labels"], imap.MailboxAttrNoSelect) {
		t.Fatalf("list = %v", names)
	}
}

func TestQuotaRejectsAppend(t *testing.T) {
	h := newHarness(t)
	if _, err := h.client.Mailbox.UpdateOneID(h.mbox.ID).SetQuotaBytes(10).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := h.dial(nil)
	raw := testutil.RawMessage("too big")
	append := client.Append("INBOX", int64(len(raw)), nil)
	_, _ = append.Write(raw)
	_ = append.Close()
	if _, err := append.Wait(); err == nil || !strings.Contains(strings.ToUpper(err.Error()), "QUOTA") {
		t.Fatalf("expected OVERQUOTA, got %v", err)
	}
}

func containsFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

func containsAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if attr == want {
			return true
		}
	}
	return false
}

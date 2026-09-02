package mailstore_test

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func seedMessage(t *testing.T, client *ent.Client, size int64) *ent.Message {
	t.Helper()
	msg, err := client.Message.Create().SetID(ids.New().String()).SetTraceID(ids.New().String()).SetBlobKey("blob-" + ids.New().String()).SetSha256("sha").SetSizeBytes(size).Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestAttachAllocatesMonotonicUIDs(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "uid@example.com")
	inbox, err := store.FolderByName(ctx, mbox.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if inbox.UIDValidity == 0 {
		t.Fatal("system folders should get a non-zero UIDVALIDITY")
	}
	var uids []uint32
	for range 3 {
		item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 10).ID, FolderID: inbox.ID, SizeBytes: 10})
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, item.UID)
	}
	if uids[0] != 1 || uids[1] != 2 || uids[2] != 3 {
		t.Fatalf("uids = %v, want 1,2,3", uids)
	}
	folder, _ := client.Folder.Get(ctx, inbox.ID)
	if folder.UIDNext != 4 {
		t.Fatalf("uid_next = %d, want 4", folder.UIDNext)
	}
	items, _ := store.ActiveMessagesInFolder(ctx, inbox.ID)
	if err := store.SoftDelete(ctx, items[1]); err != nil {
		t.Fatal(err)
	}
	item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 10).ID, FolderID: inbox.ID, SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if item.UID != 4 {
		t.Fatalf("uid after expunge = %d, want 4 (UIDs must never be reused)", item.UID)
	}
}

func TestQuotaAccountingRoundTrips(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "quota@example.com")
	inbox, _ := store.FolderByName(ctx, mbox.ID, "INBOX")
	trash, _ := store.FolderByName(ctx, mbox.ID, "Trash")

	item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 500).ID, FolderID: inbox.ID, SizeBytes: 500})
	if err != nil {
		t.Fatal(err)
	}
	used := func() int64 {
		m, _ := client.Mailbox.Get(ctx, mbox.ID)
		return m.UsedBytes
	}
	if used() != 500 {
		t.Fatalf("used after attach = %d", used())
	}
	moved, err := store.Move(ctx, item, trash.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used() != 500 {
		t.Fatalf("move must not change usage, got %d", used())
	}
	if moved.UID != 1 || moved.FolderID != trash.ID {
		t.Fatalf("moved item = uid %d folder %s", moved.UID, moved.FolderID)
	}
	if err := store.SoftDelete(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if used() != 0 {
		t.Fatalf("used after delete = %d, want 0", used())
	}
	if err := store.SoftDelete(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if used() != 0 {
		t.Fatalf("double delete must be idempotent, got %d", used())
	}
	deleted, _ := client.MailboxMessage.Get(ctx, moved.ID)
	restored, err := store.Move(ctx, deleted, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.DeletedAt != nil || used() != 500 {
		t.Fatalf("restore should re-add usage: deleted=%v used=%d", restored.DeletedAt, used())
	}
}

func TestAttachEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "limit@example.com")
	if _, err := client.Mailbox.UpdateOneID(mbox.ID).SetQuotaBytes(100).Save(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, _ := store.FolderByName(ctx, mbox.ID, "INBOX")
	if _, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 80).ID, FolderID: inbox.ID, SizeBytes: 80, EnforceQuota: true}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 30).ID, FolderID: inbox.ID, SizeBytes: 30, EnforceQuota: true})
	if err != mailstore.ErrOverQuota {
		t.Fatalf("expected ErrOverQuota, got %v", err)
	}
}

func TestLabelsGetTheirOwnUIDSpace(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "labels@example.com")
	inbox, _ := store.FolderByName(ctx, mbox.ID, "INBOX")
	label, err := store.CreateLabel(ctx, mbox.ID, "Work")
	if err != nil {
		t.Fatal(err)
	}
	var items []*ent.MailboxMessage
	for range 2 {
		item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 1).ID, FolderID: inbox.ID, SizeBytes: 1})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	first, err := store.AddLabel(ctx, items[1], label.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.AddLabel(ctx, items[1], label.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.UID != 1 || again.UID != 1 {
		t.Fatalf("label uids = %d, %d; want both 1", first.UID, again.UID)
	}
	second, _ := store.AddLabel(ctx, items[0], label.ID)
	if second.UID != 2 {
		t.Fatalf("second label uid = %d, want 2", second.UID)
	}
	if err := store.RemoveLabel(ctx, items[1], label.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveLabel(ctx, items[1], label.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNotifierDeliversChangesToSubscribers(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "notify@example.com")
	inbox, _ := store.FolderByName(ctx, mbox.ID, "INBOX")
	changes, cancel := store.Notifier().Subscribe(mailstore.FolderContainer(inbox.ID))
	defer cancel()
	item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 1).ID, FolderID: inbox.ID, SizeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case change := <-changes:
		if change.Kind != mailstore.ChangeExists || change.UID != item.UID {
			t.Fatalf("unexpected change %+v", change)
		}
	default:
		t.Fatal("expected an exists notification")
	}
	flags := mailstore.FlagsOf(item)
	flags.Seen = true
	flags.Keywords = []string{"$Important"}
	if _, err := store.SetFlags(ctx, item, flags); err != nil {
		t.Fatal(err)
	}
	change := <-changes
	if change.Kind != mailstore.ChangeFlags || len(change.Flags) != 2 {
		t.Fatalf("unexpected flag change %+v", change)
	}
}

func TestRecomputeUsedBytesRepairsDrift(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	mbox := testutil.SeedMailbox(t, store, "drift@example.com")
	inbox, _ := store.FolderByName(ctx, mbox.ID, "INBOX")
	if _, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: seedMessage(t, client, 40).ID, FolderID: inbox.ID, SizeBytes: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Mailbox.UpdateOneID(mbox.ID).SetUsedBytes(9999).Save(ctx); err != nil {
		t.Fatal(err)
	}
	total, err := store.RecomputeUsedBytes(ctx, mbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 40 {
		t.Fatalf("recomputed = %d, want 40", total)
	}
}

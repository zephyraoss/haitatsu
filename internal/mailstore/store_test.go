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

func TestSoftDeleteManyBatchesAcrossMailboxes(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	first := testutil.SeedMailbox(t, store, "batch-a@example.com")
	second := testutil.SeedMailbox(t, store, "batch-b@example.com")
	firstInbox, _ := store.FolderByName(ctx, first.ID, "INBOX")
	firstArchive, _ := store.FolderByName(ctx, first.ID, "Archive")
	secondInbox, _ := store.FolderByName(ctx, second.ID, "INBOX")
	shared := seedMessage(t, client, 40)
	attach := func(mailboxID, folderID, messageID string, size int64) *ent.MailboxMessage {
		item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mailboxID, MessageID: messageID, FolderID: folderID, SizeBytes: size})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	a1 := attach(first.ID, firstInbox.ID, shared.ID, 40)
	a2 := attach(first.ID, firstArchive.ID, seedMessage(t, client, 20).ID, 20)
	a3 := attach(first.ID, firstInbox.ID, seedMessage(t, client, 7).ID, 7)
	keep := attach(first.ID, firstInbox.ID, seedMessage(t, client, 3).ID, 3)
	b1 := attach(second.ID, secondInbox.ID, shared.ID, 40)
	b2 := attach(second.ID, secondInbox.ID, seedMessage(t, client, 11).ID, 11)
	if err := store.SoftDelete(ctx, b2); err != nil {
		t.Fatal(err)
	}
	changes, cancel := store.Notifier().Subscribe(mailstore.FolderContainer(firstInbox.ID))
	defer cancel()
	if err := store.SoftDeleteMany(ctx, []*ent.MailboxMessage{a1, a2, b1, a3, b2}); err != nil {
		t.Fatal(err)
	}
	used := func(id string) int64 {
		m, _ := client.Mailbox.Get(ctx, id)
		return m.UsedBytes
	}
	if got := used(first.ID); got != 3 {
		t.Fatalf("first used_bytes = %d, want 3", got)
	}
	if got := used(second.ID); got != 0 {
		t.Fatalf("second used_bytes = %d, want 0", got)
	}
	for _, item := range []*ent.MailboxMessage{a1, a2, a3, b1} {
		row, _ := client.MailboxMessage.Get(ctx, item.ID)
		if row.DeletedAt == nil {
			t.Fatalf("mailbox message %s should be soft deleted", item.ID)
		}
	}
	if row, _ := client.MailboxMessage.Get(ctx, keep.ID); row.DeletedAt != nil {
		t.Fatal("untargeted message must stay active")
	}
	var uids []uint32
	for range 2 {
		select {
		case change := <-changes:
			if change.Kind != mailstore.ChangeExpunge {
				t.Fatalf("unexpected change %+v", change)
			}
			uids = append(uids, change.UID)
		default:
			t.Fatal("expected an expunge notification")
		}
	}
	if uids[0] != a1.UID || uids[1] != a3.UID {
		t.Fatalf("expunge uids = %v, want [%d %d]", uids, a1.UID, a3.UID)
	}
	if err := store.SoftDeleteMany(ctx, []*ent.MailboxMessage{a1, a3}); err != nil {
		t.Fatal(err)
	}
	if got := used(first.ID); got != 3 {
		t.Fatalf("repeated soft delete must be idempotent, used_bytes = %d", got)
	}
}

func TestPurgeMailboxRemovesOnlyItsLabelLinks(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	gone := testutil.SeedMailbox(t, store, "purge@example.com")
	kept := testutil.SeedMailbox(t, store, "kept@example.com")
	seedLabeled := func(mailboxID string) {
		inbox, _ := store.FolderByName(ctx, mailboxID, "INBOX")
		label, err := store.CreateLabel(ctx, mailboxID, "Work")
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mailboxID, MessageID: seedMessage(t, client, 1).ID, FolderID: inbox.ID, SizeBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AddLabel(ctx, item, label.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedLabeled(gone.ID)
	seedLabeled(kept.ID)
	if err := store.PurgeMailbox(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if count, _ := client.MailboxMessageLabel.Query().Count(ctx); count != 3 {
		t.Fatalf("label links remaining = %d, want 3", count)
	}
	if count, _ := client.MailboxMessage.Query().Count(ctx); count != 3 {
		t.Fatalf("mailbox messages remaining = %d, want 3", count)
	}
	if _, err := client.Mailbox.Get(ctx, gone.ID); !ent.IsNotFound(err) {
		t.Fatalf("purged mailbox lookup err = %v", err)
	}
}

package cleanup

import (
	"context"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestCleanupTrashReleasesQuota(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	blobs := testutil.NewFakeStore()
	worker := New(client, blobs, store, metrics.New())
	mbox := testutil.SeedMailbox(t, store, "trash@example.test")
	trash, _ := store.FolderByName(ctx, mbox.ID, "Trash")
	msg, err := client.Message.Create().SetID(ids.New().String()).SetTraceID("t").SetBlobKey("blob").SetSha256("s").SetSizeBytes(100).SetCreatedAt(time.Now().Add(-40 * 24 * time.Hour)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = blobs.PutMessage(ctx, "blob", []byte("x"))
	item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: msg.ID, FolderID: trash.ID, SizeBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.MailboxMessage.UpdateOneID(item.ID).SetUpdatedAt(time.Now().Add(-31 * 24 * time.Hour)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := client.Mailbox.Get(ctx, mbox.ID)
	if after.UsedBytes != 0 {
		t.Fatalf("used_bytes after trash cleanup = %d", after.UsedBytes)
	}
	deleted, _ := client.MailboxMessage.Query().Where(mailboxmessage.IDEQ(item.ID)).Only(ctx)
	if deleted.DeletedAt == nil {
		t.Fatal("expired trash should be soft deleted")
	}
	if _, err := client.MailboxMessage.UpdateOneID(item.ID).SetDeletedAt(time.Now().Add(-31 * 24 * time.Hour)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := blobs.Objects["blob"]; ok {
		t.Fatal("orphaned blob should be removed after retention")
	}
	if count, _ := client.Message.Query().Count(ctx); count != 0 {
		t.Fatalf("message rows remaining = %d", count)
	}
}

func TestPurgeDeletedMailboxRemovesChildren(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	worker := New(client, testutil.NewFakeStore(), store, metrics.New())
	mbox := testutil.SeedMailbox(t, store, "gone@example.test")
	if _, err := client.Mailbox.UpdateOneID(mbox.ID).SetStatus("deleted").SetDeletedAt(time.Now().Add(-31 * 24 * time.Hour)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if count, _ := client.Folder.Query().Count(ctx); count != 0 {
		t.Fatalf("folders remaining = %d", count)
	}
	if count, _ := client.Mailbox.Query().Count(ctx); count != 0 {
		t.Fatal("mailbox should be purged")
	}
}

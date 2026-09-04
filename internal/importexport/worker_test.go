package importexport

import (
	"archive/zip"
	"bytes"
	"context"
	stdsql "database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"

	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	entfolder "github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type fakeStore struct {
	objects map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}}
}

func (s *fakeStore) GetMessage(_ context.Context, key string) ([]byte, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return data, nil
}

func (s *fakeStore) GetObjectReader(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStore) PutMessage(_ context.Context, key string, data []byte) error {
	s.objects[key] = data
	return nil
}

func (s *fakeStore) PutExportStream(_ context.Context, key string, data io.Reader, _ int64) error {
	buf, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	s.objects[key] = buf
	return nil
}

func newTestClient(t *testing.T) *ent.Client {
	t.Helper()
	db, err := stdsql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=foreign_keys(1)", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() { client.Close() })
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	return client
}

func seedMailbox(t *testing.T, client *ent.Client) *ent.Mailbox {
	t.Helper()
	ctx := context.Background()
	mbox, err := client.Mailbox.Create().SetPrimaryAddress(t.Name() + "@example.com").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"INBOX", "Sent", "Drafts", "Trash", "Junk", "Archive"} {
		if _, err := client.Folder.Create().SetMailboxID(mbox.ID).SetName(name).SetSystem(true).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return mbox
}

func rawMessage(subject string) []byte {
	return []byte("From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: " + subject + "\r\nMessage-ID: <" + subject + "@example.com>\r\n\r\nbody of " + subject + "\r\n")
}

func TestClaimSQLiteImportAndExportJobs(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewClient(t)
	imported, err := client.ImportJob.Create().SetMailboxID("mailbox").SetSourceType("maildir").SetSource(map[string]any{"path": "/mail"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := client.ExportJob.Create().SetMailboxID("mailbox").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	importWorker := &ImportWorker{db: db, client: client, workerID: "worker", backend: database.BackendSQLite}
	importJob, _, ok, err := importWorker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || importJob.ID != imported.ID {
		t.Fatalf("import claim = %+v, ok = %v", importJob, ok)
	}

	exportWorker := &ExportWorker{db: db, client: client, workerID: "worker", backend: database.BackendSQLite}
	exportJob, _, ok, err := exportWorker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || exportJob.ID != exported.ID {
		t.Fatalf("export claim = %+v, ok = %v", exportJob, ok)
	}
}

func writeMaildirMessage(t *testing.T, root string, rel string, raw []byte) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func folderByName(t *testing.T, client *ent.Client, mailboxID string, name string) *ent.Folder {
	t.Helper()
	found, err := client.Folder.Query().Where(entfolder.MailboxIDEQ(mailboxID), entfolder.NameEQ(name)).Only(context.Background())
	if err != nil {
		t.Fatalf("folder %q: %v", name, err)
	}
	return found
}

func messagesInFolder(t *testing.T, client *ent.Client, mailboxID string, folderID string) []*ent.MailboxMessage {
	t.Helper()
	items, err := client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mailboxID), mailboxmessage.FolderIDEQ(folderID)).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestImportMaildirJob(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	worker := &ImportWorker{client: client, store: newFakeStore()}
	mbox := seedMailbox(t, client)

	root := t.TempDir()
	writeMaildirMessage(t, root, "cur/1.host:2,S", rawMessage("read-inbox"))
	writeMaildirMessage(t, root, "new/2.host", rawMessage("unread-inbox"))
	writeMaildirMessage(t, root, ".Sent/cur/3.host:2,FS", rawMessage("sent"))
	writeMaildirMessage(t, root, ".Clients.Acme/new/4.host", rawMessage("client"))

	job := importJob{MailboxID: mbox.ID, SourceType: "maildir", Source: map[string]any{"path": root}}
	count, err := worker.importJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("imported %d, want 4", count)
	}

	inbox := folderByName(t, client, mbox.ID, "INBOX")
	inboxItems := messagesInFolder(t, client, mbox.ID, inbox.ID)
	if len(inboxItems) != 2 {
		t.Fatalf("INBOX has %d messages, want 2", len(inboxItems))
	}
	readCount := 0
	for _, item := range inboxItems {
		if item.Read {
			readCount++
		}
	}
	if readCount != 1 {
		t.Errorf("INBOX read messages = %d, want 1", readCount)
	}

	sent := folderByName(t, client, mbox.ID, "Sent")
	if !sent.System {
		t.Error("Sent import should reuse the system folder")
	}
	sentItems := messagesInFolder(t, client, mbox.ID, sent.ID)
	if len(sentItems) != 1 || !sentItems[0].Read || !sentItems[0].Flagged {
		t.Errorf("Sent items = %+v, want one read+flagged message", sentItems)
	}

	acme := folderByName(t, client, mbox.ID, "Clients/Acme")
	if acme.System {
		t.Error("created folder should not be a system folder")
	}
	if items := messagesInFolder(t, client, mbox.ID, acme.ID); len(items) != 1 {
		t.Errorf("Clients/Acme has %d messages, want 1", len(items))
	}

	usedAfterFirst := reloadMailbox(t, client, mbox.ID).UsedBytes
	if usedAfterFirst == 0 {
		t.Error("used_bytes should grow after import")
	}

	again, err := worker.importJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second import created %d messages, want 0", again)
	}
	total, err := client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mbox.ID)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("total messages after re-import = %d, want 4", total)
	}
	if used := reloadMailbox(t, client, mbox.ID).UsedBytes; used != usedAfterFirst {
		t.Errorf("used_bytes changed on re-import: %d -> %d", usedAfterFirst, used)
	}
}

func reloadMailbox(t *testing.T, client *ent.Client, id string) *ent.Mailbox {
	t.Helper()
	mbox, err := client.Mailbox.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return mbox
}

func TestImportZipJob(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	store := newFakeStore()
	worker := &ImportWorker{client: client, store: store}
	mbox := seedMailbox(t, client)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"one.eml", "two.eml"} {
		file, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(rawMessage(name)); err != nil {
			t.Fatal(err)
		}
	}
	ignored, err := zw.Create("notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ignored.Write([]byte("not a message")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	store.objects["import.zip"] = buf.Bytes()

	job := importJob{MailboxID: mbox.ID, SourceType: "zip", Source: map[string]any{"object_key": "import.zip"}}
	count, err := worker.importJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("imported %d, want 2", count)
	}
	inbox := folderByName(t, client, mbox.ID, "INBOX")
	if items := messagesInFolder(t, client, mbox.ID, inbox.ID); len(items) != 2 {
		t.Errorf("INBOX has %d messages, want 2", len(items))
	}
}

func TestAddToMailboxSkipsDuplicateMessageRows(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	store := newFakeStore()
	worker := &ImportWorker{client: client, store: store}
	mbox := seedMailbox(t, client)
	inbox := folderByName(t, client, mbox.ID, "INBOX")

	raw := rawMessage("dupe")
	created, err := worker.addToMailbox(ctx, mbox.ID, inbox.ID, mbox.PrimaryAddress, raw, importFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first add should create")
	}

	duplicateID := ids.New().String()
	if err := store.PutMessage(ctx, "dupe-blob", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Message.Create().
		SetID(duplicateID).
		SetTraceID(ids.New().String()).
		SetBlobKey("dupe-blob").
		SetSha256(sha256Hex(raw)).
		SetSizeBytes(int64(len(raw))).
		Save(ctx); err != nil {
		t.Fatal(err)
	}

	created, err = worker.addToMailbox(ctx, mbox.ID, inbox.ID, mbox.PrimaryAddress, raw, importFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("duplicate content should not attach again")
	}
	total, err := client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mbox.ID)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("mailbox has %d attachments, want 1", total)
	}
}

func TestImportJobPersistsProgress(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	worker := &ImportWorker{client: client, store: newFakeStore()}
	mbox := seedMailbox(t, client)

	root := t.TempDir()
	for i := range 3 {
		writeMaildirMessage(t, root, fmt.Sprintf("cur/%d.host", i), rawMessage(fmt.Sprintf("progress-%d", i)))
	}
	source := map[string]any{"path": root}
	row, err := client.ImportJob.Create().SetMailboxID(mbox.ID).SetSourceType("maildir").SetSource(source).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	count, err := worker.importJob(ctx, importJob{ID: row.ID, MailboxID: mbox.ID, SourceType: "maildir", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("imported %d, want 3", count)
	}
	reloaded, err := client.ImportJob.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ImportedCount != 3 {
		t.Errorf("imported_count = %d, want 3", reloaded.ImportedCount)
	}
}

func TestExportBuildZIP(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	store := newFakeStore()
	importWorker := &ImportWorker{client: client, store: store}
	mbox := seedMailbox(t, client)

	root := t.TempDir()
	writeMaildirMessage(t, root, "cur/1.host", rawMessage("first"))
	writeMaildirMessage(t, root, "cur/2.host", rawMessage("second"))
	if _, err := importWorker.importJob(ctx, importJob{MailboxID: mbox.ID, SourceType: "maildir", Source: map[string]any{"path": root}}); err != nil {
		t.Fatal(err)
	}

	exportWorker := &ExportWorker{client: client, store: store}
	archive, size, err := exportWorker.buildZIP(ctx, mbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 2 {
		t.Errorf("export contains %d files, want 2", len(reader.File))
	}
}

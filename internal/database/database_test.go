package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestSQLiteOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	client, err := Open(ctx, config.DatabaseConfig{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "haitatsu.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if !client.MigrationsDone() {
		t.Fatal("migrations were not marked complete")
	}
	if client.Backend() != BackendSQLite || client.Dialect() != "sqlite3" {
		t.Fatalf("backend = %q, dialect = %q", client.Backend(), client.Dialect())
	}

	var migrationCount int
	if err := client.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 4 {
		t.Fatalf("migration count = %d, want 4", migrationCount)
	}

	if bus := NewChangeBus(client.db, client.backend); bus != nil {
		t.Fatal("local SQLite should use in-process notifications only")
	}
	bus := &pollingChangeBus{db: client.db}
	lastID, err := bus.latestID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"kind":"exists"}`)
	if err := bus.Publish(ctx, payload); err != nil {
		t.Fatal(err)
	}
	var got []byte
	lastID, err = bus.poll(ctx, lastID, func(value []byte) { got = append([]byte(nil), value...) })
	if err != nil {
		t.Fatal(err)
	}
	if lastID == 0 || string(got) != string(payload) {
		t.Fatalf("last id = %d, payload = %q", lastID, got)
	}
}

func TestSQLiteMigrationsAreRepeatable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "haitatsu.db")
	for range 2 {
		client, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.RunMigrations(ctx); err != nil {
			client.Close()
			t.Fatal(err)
		}
		client.Close()
	}
}

func TestSQLiteUIDBackfillMigration(t *testing.T) {
	ctx := context.Background()
	client, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "haitatsu.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}

	createdAt := time.Date(2025, 7, 4, 12, 30, 0, 0, time.UTC)
	mailbox, err := client.Ent().Mailbox.Create().SetPrimaryAddress("backfill@example.test").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := client.Ent().Folder.Create().SetMailboxID(mailbox.ID).SetName("INBOX").SetUIDValidity(1).SetCreatedAt(createdAt).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.Ent().Message.Create().SetTraceID("trace").SetBlobKey("message.eml").SetSha256("sha256").SetSizeBytes(10).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mailboxMessage, err := client.Ent().MailboxMessage.Create().SetMailboxID(mailbox.ID).SetMessageID(message.ID).SetFolderID(folder.ID).SetUID(0).SetOriginalRcpt(mailbox.PrimaryAddress).SetBaseRcpt(mailbox.PrimaryAddress).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := client.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := versionedMigrations(BackendSQLite)[1].Apply(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	folder, err = client.Ent().Folder.Get(ctx, folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	mailboxMessage, err = client.Ent().MailboxMessage.Get(ctx, mailboxMessage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if folder.UIDValidity != uint32(createdAt.Unix()) || folder.UIDNext != 2 || mailboxMessage.UID != 1 {
		t.Fatalf("folder = %+v, message uid = %d", folder, mailboxMessage.UID)
	}
}

func TestLibSQLConnectorUsesSQLiteDialect(t *testing.T) {
	ctx := context.Background()
	client, err := Open(ctx, config.DatabaseConfig{
		Driver: "libsql",
		DSN:    "file:" + filepath.Join(t.TempDir(), "haitatsu.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if client.Backend() != BackendLibSQL || client.Dialect() != "sqlite3" {
		t.Fatalf("backend = %q, dialect = %q", client.Backend(), client.Dialect())
	}
}

func TestLibSQLRemote(t *testing.T) {
	endpoint := os.Getenv("HAITATSU_TEST_LIBSQL_URL")
	if endpoint == "" {
		t.Skip("HAITATSU_TEST_LIBSQL_URL is not set")
	}
	ctx := context.Background()
	client, err := Open(ctx, config.DatabaseConfig{
		Driver:    "libsql",
		DSN:       endpoint,
		AuthToken: os.Getenv("HAITATSU_TEST_LIBSQL_AUTH_TOKEN"),
		Namespace: os.Getenv("HAITATSU_TEST_LIBSQL_NAMESPACE"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	address := fmt.Sprintf("libsql-test-%d@example.test", time.Now().UnixNano())
	mailbox, err := client.Ent().Mailbox.Create().SetPrimaryAddress(address).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ent().Mailbox.Get(ctx, mailbox.ID); err != nil {
		t.Fatal(err)
	}
	message, err := client.Ent().Message.Create().SetTraceID("trace").SetBlobKey("message.eml").SetSha256("sha256").SetSizeBytes(10).SetSubject("remote search needle").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := client.db.QueryRowContext(ctx, `SELECT messages.id FROM messages JOIN messages_search ON messages_search.message_id = messages.id WHERE messages_search MATCH ?`, `"needle"`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if messageID != message.ID {
		t.Fatalf("search result = %q, want %q", messageID, message.ID)
	}

	bus, ok := NewChangeBus(client.db, client.backend).(*pollingChangeBus)
	if !ok {
		t.Fatal("libSQL did not use the polling change bus")
	}
	lastID, err := bus.latestID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, []byte(`{"kind":"exists"}`)); err != nil {
		t.Fatal(err)
	}
	delivered := false
	if _, err := bus.poll(ctx, lastID, func([]byte) { delivered = true }); err != nil {
		t.Fatal(err)
	}
	if !delivered {
		t.Fatal("libSQL change bus did not deliver the published change")
	}
}

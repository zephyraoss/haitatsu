package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestMailboxSpamThresholdsUpgrade(t *testing.T) {
	ctx := context.Background()
	client, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "mailboxes.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	mbox, err := client.Ent().Mailbox.Create().SetPrimaryAddress("existing@example.test").SetQuotaBytes(1234).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the mailbox table used before per-inbox thresholds existed.
	if _, err := client.db.ExecContext(ctx, "ALTER TABLE mailboxes DROP COLUMN spam_thresholds"); err != nil {
		t.Fatal(err)
	}
	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := client.Ent().Mailbox.Get(ctx, mbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrimaryAddress != mbox.PrimaryAddress || got.QuotaBytes != 1234 || len(got.SpamThresholds) != 0 {
		t.Fatalf("upgrade changed existing mailbox settings: %+v", got)
	}
	if _, err := client.Ent().Mailbox.UpdateOneID(mbox.ID).SetSpamThresholds(map[string]float64{"junk_threshold": 0, "spam_threshold": 0.9}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = client.Ent().Mailbox.Get(ctx, mbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if junk, exists := got.SpamThresholds["junk_threshold"]; !exists || junk != 0 || got.SpamThresholds["spam_threshold"] != 0.9 {
		t.Fatalf("thresholds did not persist through migration: %+v", got.SpamThresholds)
	}
	if _, err := client.Ent().Mailbox.UpdateOneID(mbox.ID).ClearSpamThresholds().Save(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = client.Ent().Mailbox.Get(ctx, mbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SpamThresholds) != 0 {
		t.Fatalf("thresholds not cleared: %+v", got.SpamThresholds)
	}
}

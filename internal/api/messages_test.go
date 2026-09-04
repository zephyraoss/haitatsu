package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"entgo.io/ent/dialect"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
)

func TestMessageSearchWhereTermsClause(t *testing.T) {
	where, args := messageSearchWhere(messageSearch{Terms: []string{"inbound"}}, dialect.Postgres)
	if len(where) != 1 {
		t.Fatalf("where len = %d, want 1", len(where))
	}
	if got := where[0]; got != messageSearchVector+" @@ query" {
		t.Fatalf("where[0] = %q", got)
	}
	if len(args) != 1 || args[0] != "inbound" {
		t.Fatalf("args = %#v, want [inbound]", args)
	}
}

func TestMessageSearchWhereFromUsesPlaceholder(t *testing.T) {
	where, args := messageSearchWhere(messageSearch{From: "alice"}, dialect.Postgres)
	if len(where) != 1 {
		t.Fatalf("where len = %d, want 1", len(where))
	}
	if got := where[0]; got != "from_addresses::text ILIKE '%' || $1 || '%'" {
		t.Fatalf("where[0] = %q", got)
	}
	if len(args) != 1 || args[0] != "alice" {
		t.Fatalf("args = %#v, want [alice]", args)
	}
}

func TestMessageSearchQueryUsesSQLiteSyntax(t *testing.T) {
	query, args := messageSearchQuery(messageSearch{
		Terms:         []string{"hello", `"two words"`},
		From:          "alice",
		HasAttachment: true,
	}, dialect.SQLite)
	for _, unsupported := range []string{"ILIKE", "::text", "jsonb_", "to_tsvector", "$1"} {
		if strings.Contains(query, unsupported) {
			t.Errorf("query contains PostgreSQL syntax %q: %s", unsupported, query)
		}
	}
	if len(args) != 2 || args[0] != `"hello" AND "two words"` || args[1] != "alice" {
		t.Fatalf("args = %#v", args)
	}
}

func TestMessageSearchQueryRunsOnSQLite(t *testing.T) {
	ctx := context.Background()
	dbClient, err := database.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "haitatsu.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer dbClient.Close()
	if err := dbClient.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	message, err := dbClient.Ent().Message.Create().
		SetTraceID("trace").
		SetBlobKey("message.eml").
		SetSha256("sha256").
		SetSizeBytes(10).
		SetSubject("Quarterly Invoice").
		SetFromAddresses([]string{"alice@example.test"}).
		SetAttachments([]map[string]any{{"filename": "invoice.pdf"}}).
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	query, args := messageSearchQuery(messageSearch{Terms: []string{"invoice"}, From: "ALICE", HasAttachment: true}, dialect.SQLite)
	rows, err := dbClient.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("expected message %s in search results", message.ID)
	}
	var id string
	if err := rows.Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != message.ID {
		t.Fatalf("message id = %q, want %q", id, message.ID)
	}
}

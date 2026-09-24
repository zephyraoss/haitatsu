package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/testutil"
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

func TestDownloadMessageMissingReturnsNotFound(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	request := func(path string, status int) []byte {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != status {
			t.Fatalf("GET %s: status=%d want=%d body=%s", path, res.StatusCode, status, raw)
		}
		return raw
	}

	for _, path := range []string{"/messages/does-not-exist/raw", "/messages/does-not-exist/attachments/1"} {
		var body ErrorBody
		if err := json.Unmarshal(request(path, fiber.StatusNotFound), &body); err != nil {
			t.Fatalf("GET %s: invalid response: %v", path, err)
		}
		if body.Error.Code != "message_not_found" {
			t.Fatalf("GET %s: error code = %q, want message_not_found", path, body.Error.Code)
		}
	}
}

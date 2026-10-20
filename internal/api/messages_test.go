package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
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

func TestMessageSearchClauseUsesSQLiteSyntax(t *testing.T) {
	clause, args := messageSearchClause(messageSearch{
		Terms:         []string{"hello", `"two words"`},
		From:          "alice",
		HasAttachment: true,
	}, dialect.SQLite, "mailbox_messages.message_id")
	for _, unsupported := range []string{"ILIKE", "::text", "jsonb_", "to_tsvector", "$1"} {
		if strings.Contains(clause, unsupported) {
			t.Errorf("clause contains PostgreSQL syntax %q: %s", unsupported, clause)
		}
	}
	if len(args) != 2 || args[0] != `"hello" AND "two words"` || args[1] != "alice" {
		t.Fatalf("args = %#v", args)
	}
}

func TestListMessagesSearchScopedToMailbox(t *testing.T) {
	ctx := context.Background()
	dbClient, err := database.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "haitatsu.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer dbClient.Close()
	if err := dbClient.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	client := dbClient.Ent()
	store := testutil.NewMailStore(t, client)
	alice := testutil.SeedMailbox(t, store, "alice@example.test")
	bob := testutil.SeedMailbox(t, store, "bob@example.test")
	for _, mbox := range []*ent.Mailbox{alice, bob} {
		subject := "Quarterly Invoice"
		if mbox.ID == bob.ID {
			subject = "Weekly Report"
		}
		message, err := client.Message.Create().
			SetTraceID("trace-" + mbox.ID).
			SetBlobKey(mbox.ID + ".eml").
			SetSha256("sha256-" + mbox.ID).
			SetSizeBytes(10).
			SetSubject(subject).
			SetFromAddresses([]string{"carol@example.test"}).
			SetAttachments([]map[string]any{{"filename": "doc.pdf"}}).
			Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := store.FolderByName(ctx, mbox.ID, "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: message.ID, FolderID: inbox.ID, SizeBytes: 10}); err != nil {
			t.Fatal(err)
		}
	}

	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	cfg.Database.Driver = "sqlite"
	Register(app, client, dbClient.SQL(), testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	count := func(mailboxID string, q string) int {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1/mailboxes/"+mailboxID+"/messages?q="+url.QueryEscape(q), nil)
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		return len(body.Data)
	}

	if got := count(alice.ID, "invoice from:CAROL has:attachment"); got != 1 {
		t.Fatalf("alice invoice results = %d, want 1", got)
	}
	if got := count(alice.ID, "report"); got != 0 {
		t.Fatalf("alice report results = %d, want 0", got)
	}
	if got := count(bob.ID, "invoice"); got != 0 {
		t.Fatalf("bob invoice results = %d, want 0", got)
	}
	if got := count(alice.ID, "nomatch"); got != 0 {
		t.Fatalf("alice nomatch results = %d, want 0", got)
	}
}

func TestMessageSearchPredicateRenumbersPostgresPlaceholders(t *testing.T) {
	selector := entsql.Dialect(dialect.Postgres).Select("id").From(entsql.Table("mailbox_messages"))
	selector.Where(entsql.EQ(selector.C("mailbox_id"), "mbox"))
	messageSearchPredicate(messageSearch{Terms: []string{"invoice"}, From: "alice"}, dialect.Postgres)(selector)
	query, args := selector.Query()
	if !strings.Contains(query, "websearch_to_tsquery('english', $2)") || !strings.Contains(query, "|| $3 ||") {
		t.Fatalf("query = %s", query)
	}
	if len(args) != 3 || args[0] != "mbox" || args[1] != "invoice" || args[2] != "alice" {
		t.Fatalf("args = %#v", args)
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

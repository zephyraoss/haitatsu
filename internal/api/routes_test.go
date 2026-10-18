package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestNormalizeAddress(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		" User@Example.COM ":       {"user@example.com", true},
		"user@example.com":         {"user@example.com", true},
		"User <user@example.com>":  {"", false},
		"not-an-address":           {"", false},
		"a@b@example.com":          {"", false},
		"user@example.com, x@y.co": {"", false},
	}
	for input, want := range cases {
		got, ok := normalizeAddress(input)
		if ok != want.ok || (ok && got != want.want) {
			t.Errorf("normalizeAddress(%q) = %q, %v; want %q, %v", input, got, ok, want.want, want.ok)
		}
	}
}

func TestRevokeAppPasswordScopedToMailbox(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	owner := testutil.SeedMailbox(t, store, "owner@example.test")
	other := testutil.SeedMailbox(t, store, "other@example.test")
	item, err := client.AppPassword.Create().SetMailboxID(owner.ID).SetName("mail").SetHash("x").SetScopes([]string{"imap"}).Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	req := httptest.NewRequest("DELETE", "/api/v1/mailboxes/"+other.ID+"/passwords/"+item.ID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	fresh, err := client.AppPassword.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RevokedAt != nil {
		t.Fatal("app password revoked through another mailbox")
	}
}

func TestCreateMailboxNormalizesAddressAndCreatesFolders(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	req := httptest.NewRequest("POST", "/api/v1/mailboxes", strings.NewReader(`{"primary_address":" Alice@Example.TEST "}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	mbox, err := client.Mailbox.Query().Where(mailbox.PrimaryAddressEQ("alice@example.test")).Only(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.Folder.Query().Where(folder.MailboxIDEQ(mbox.ID)).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("default folders were not created")
	}
}

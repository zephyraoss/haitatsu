package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent/apppassword"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestCreateAppPasswordWithChosenPassword(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	owner := testutil.SeedMailbox(t, store, "owner@example.test")
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	post := func(body string) (int, appPasswordResponse) {
		req := httptest.NewRequest("POST", "/api/v1/mailboxes/"+owner.ID+"/passwords", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var envelope struct {
			Data appPasswordResponse `json:"data"`
		}
		_ = json.NewDecoder(res.Body).Decode(&envelope)
		return res.StatusCode, envelope.Data
	}

	status, created := post(`{"name":"mail","scopes":["imap","smtp"],"password":"correct horse battery"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	if created.Password != "correct horse battery" {
		t.Fatalf("password = %q, want the chosen password", created.Password)
	}
	stored, err := client.AppPassword.Query().Where(apppassword.IDEQ(created.ID)).Only(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := passwordauth.VerifyPassword("correct horse battery", stored.Hash); err != nil || !valid {
		t.Fatalf("chosen password does not verify: %v %v", valid, err)
	}

	if status, _ := post(`{"name":"mail","scopes":["imap"],"password":"short"}`); status != fiber.StatusBadRequest {
		t.Fatalf("short password status = %d, want 400", status)
	}
	if status, _ := post(`{"name":"mail","scopes":["imap"],"password":"correct\u0000horse battery"}`); status != fiber.StatusBadRequest {
		t.Fatalf("control character password status = %d, want 400", status)
	}

	status, generated := post(`{"name":"mail","scopes":["imap"]}`)
	if status != fiber.StatusCreated || generated.Password == "" || generated.Password == created.Password {
		t.Fatalf("generated password status = %d, password = %q", status, generated.Password)
	}
}

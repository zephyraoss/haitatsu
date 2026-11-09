package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestDeleteDKIMKeyAllowsDomainToBeAddedAgain(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	call := func(method string, path string, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		return res.StatusCode, decoded
	}

	status, body := call("POST", "/api/v1/dkim", `{"domain":"example.test"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	id := body["data"].(map[string]any)["id"].(string)
	if status, _ := call("POST", "/api/v1/dkim", `{"domain":"example.test"}`); status != fiber.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", status)
	}
	if _, body := call("GET", "/api/v1/dkim?domain=Example.test", ""); len(body["data"].([]any)) != 1 {
		t.Fatalf("domain filter returned %v", body["data"])
	}
	if _, body := call("GET", "/api/v1/dkim?domain=other.test", ""); len(body["data"].([]any)) != 0 {
		t.Fatalf("domain filter returned %v for other domain", body["data"])
	}
	if status, _ := call("DELETE", "/api/v1/dkim/"+id, ""); status != fiber.StatusOK {
		t.Fatalf("delete status = %d", status)
	}
	if status, _ := call("DELETE", "/api/v1/dkim/"+id, ""); status != fiber.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", status)
	}
	if status, _ := call("POST", "/api/v1/dkim", `{"domain":"example.test"}`); status != fiber.StatusCreated {
		t.Fatalf("re-create status = %d", status)
	}
}

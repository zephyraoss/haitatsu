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
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func newSMTPCredentialApp(t *testing.T) (*fiber.App, *ent.Client) {
	t.Helper()
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	for _, domain := range []string{"example.test", "other.test"} {
		if _, err := client.DKIMKey.Create().SetDomain(domain).SetPrivateKeyPem("x").SetPublicKeyPem("y").Save(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)
	return app, client
}

func callAPI(t *testing.T, app *fiber.App, method string, path string, body string) (int, map[string]any) {
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

func TestCreateSMTPCredentialReturnsUsernameAndPasswordOnce(t *testing.T) {
	app, client := newSMTPCredentialApp(t)
	status, body := callAPI(t, app, "POST", "/api/v1/smtp-credentials", `{"name":"app","domains":[" Example.TEST ","other.test","example.test"]}`)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d body = %v", status, body)
	}
	created := body["data"].(map[string]any)
	username, _ := created["username"].(string)
	password, _ := created["password"].(string)
	if !strings.HasPrefix(username, "smtp_") || password == "" {
		t.Fatalf("created = %v", created)
	}
	stored, err := client.SMTPCredential.Get(context.Background(), created["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stored.Domains, ",") != "example.test,other.test" {
		t.Fatalf("domains = %v", stored.Domains)
	}
	if ok, _ := passwordauth.VerifyPassword(password, stored.Hash); !ok {
		t.Fatal("stored hash does not match returned password")
	}
	status, body = callAPI(t, app, "GET", "/api/v1/smtp-credentials/"+stored.ID, "")
	if status != fiber.StatusOK || body["data"].(map[string]any)["password"] != nil {
		t.Fatalf("get leaked password or failed: %d %v", status, body)
	}
}

func TestCreateSMTPCredentialRequiresDKIMForEveryDomain(t *testing.T) {
	app, client := newSMTPCredentialApp(t)
	status, body := callAPI(t, app, "POST", "/api/v1/smtp-credentials", `{"name":"app","domains":["example.test","unsigned.test"]}`)
	if status != fiber.StatusUnprocessableEntity || !strings.Contains(body["error"].(map[string]any)["message"].(string), "unsigned.test") {
		t.Fatalf("status = %d body = %v", status, body)
	}
	for _, payload := range []string{`{"name":"app","domains":[]}`, `{"name":"","domains":["example.test"]}`, `{"name":"app","domains":["not a domain"]}`} {
		if status, _ := callAPI(t, app, "POST", "/api/v1/smtp-credentials", payload); status != fiber.StatusBadRequest {
			t.Fatalf("%s: status = %d", payload, status)
		}
	}
	if count, _ := client.SMTPCredential.Query().Count(context.Background()); count != 0 {
		t.Fatalf("created %d credentials", count)
	}
}

func TestRevokeSMTPCredential(t *testing.T) {
	app, client := newSMTPCredentialApp(t)
	item, err := client.SMTPCredential.Create().SetUsername("smtp_x").SetName("app").SetHash("x").SetDomains([]string{"example.test"}).Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := callAPI(t, app, "DELETE", "/api/v1/smtp-credentials/"+item.ID, ""); status != fiber.StatusOK {
		t.Fatalf("status = %d", status)
	}
	fresh, _ := client.SMTPCredential.Get(context.Background(), item.ID)
	if fresh.RevokedAt == nil {
		t.Fatal("credential not revoked")
	}
	if status, _ := callAPI(t, app, "DELETE", "/api/v1/smtp-credentials/missing", ""); status != fiber.StatusNotFound {
		t.Fatalf("missing credential status = %d", status)
	}
}

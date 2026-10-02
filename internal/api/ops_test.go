package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestCheckDNSRejectsUnhostedOrInvalidDomains(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	testutil.SeedMailbox(t, store, "user@Hosted.Example")
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)

	cases := map[string]int{
		"attacker.example":   fiber.StatusNotFound,
		"internal":           fiber.StatusBadRequest,
		"bad_name.example":   fiber.StatusBadRequest,
		"HOSTED.example":     fiber.StatusOK,
		"sub.hosted.example": fiber.StatusNotFound,
		"nothosted.example.": fiber.StatusBadRequest,
		"%2e%2e.example":     fiber.StatusBadRequest,
		"hosted.example%00":  fiber.StatusBadRequest,
	}
	for domain, want := range cases {
		req := httptest.NewRequest("GET", "/api/v1/dns/check/"+domain, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("GET /dns/check/%s: status=%d want=%d", domain, res.StatusCode, want)
		}
	}
}

func TestMXTargetsInboundHost(t *testing.T) {
	inbound := []string{"haitatsu.kessoku.zpr.ax", "mx.zephyra.email"}
	cases := map[string]bool{
		"mx.zephyra.email.":        true,
		"MX.Zephyra.Email":         true,
		"haitatsu.kessoku.zpr.ax.": true,
		"mail.zephyra.email.":      false,
		"":                         false,
	}
	for target, want := range cases {
		if got := mxTargetsInboundHost(target, inbound); got != want {
			t.Errorf("mxTargetsInboundHost(%q) = %v, want %v", target, got, want)
		}
	}
}

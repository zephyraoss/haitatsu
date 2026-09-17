package api

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

func TestMailboxSpamThresholdsAPI(t *testing.T) {
	client, db := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	app := fiber.New()
	cfg := &config.Config{}
	cfg.API.ServiceToken = "test-token"
	Register(app, client, db, testutil.NewFakeStore(), store, nil, config.NewHolder(cfg), nil)
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
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
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, res.StatusCode, status, raw)
		}
		return raw
	}
	decode := func(raw []byte) *ent.Mailbox {
		t.Helper()
		var result struct {
			Data *ent.Mailbox `json:"data"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || result.Data == nil {
			t.Fatalf("invalid mailbox response %s: %v", raw, err)
		}
		return result.Data
	}
	created := decode(request("POST", "/mailboxes", `{"primary_address":"alice@example.test","spam_thresholds":{"junk_threshold":0,"spam_threshold":0.9,"phishing_threshold":1}}`, 201))
	path := "/mailboxes/" + created.ID
	want := map[string]float64{"junk_threshold": 0, "spam_threshold": 0.9, "phishing_threshold": 1}
	if !maps.Equal(created.SpamThresholds, want) {
		t.Fatalf("create thresholds=%v", created.SpamThresholds)
	}
	got := decode(request("GET", path, "", 200))
	if !maps.Equal(got.SpamThresholds, want) {
		t.Fatalf("get thresholds=%v", got.SpamThresholds)
	}
	var page struct {
		Data []*ent.Mailbox `json:"data"`
	}
	if err := json.Unmarshal(request("GET", "/mailboxes", "", 200), &page); err != nil || len(page.Data) != 1 || !maps.Equal(page.Data[0].SpamThresholds, want) {
		t.Fatalf("list=%+v err=%v", page, err)
	}
	got = decode(request("PATCH", path, `{"quota_bytes":1234}`, 200))
	if !maps.Equal(got.SpamThresholds, want) {
		t.Fatal("omitted thresholds changed existing values")
	}
	got = decode(request("PATCH", path, `{"spam_thresholds":{"junk_threshold":3}}`, 200))
	if !maps.Equal(got.SpamThresholds, map[string]float64{"junk_threshold": 3}) {
		t.Fatalf("replacement merged instead: %v", got.SpamThresholds)
	}
	for _, invalid := range []string{
		`{"junk_threshold":-1}`, `{"spam_threshold":0}`, `{"spam_threshold":1.01}`, `{"phishing_threshold":-0.1}`,
		`{"junk_threshold":null}`, `{"spam_threshold":null}`, `{"reject_threshold":9}`, `{"junk_threshold":"3"}`,
		`{"phishing_threshold":true}`, `{"junk_threshold":1e999}`, `[]`, `2`, `false`, `"invalid"`,
	} {
		t.Run(invalid, func(t *testing.T) {
			request("PATCH", path, `{"quota_bytes":999,"spam_thresholds":`+invalid+`}`, 400)
			request("POST", "/mailboxes", `{"primary_address":"invalid@example.test","spam_thresholds":`+invalid+`}`, 400)
		})
	}
	got = decode(request("GET", path, "", 200))
	if got.QuotaBytes != 1234 || !maps.Equal(got.SpamThresholds, map[string]float64{"junk_threshold": 3}) {
		t.Fatalf("invalid request changed mailbox: %+v", got)
	}
	if count, err := client.Mailbox.Query().Count(context.Background()); err != nil || count != 1 {
		t.Fatalf("invalid create persisted: count=%d err=%v", count, err)
	}
	for _, clear := range []string{"null", "{}"} {
		request("PATCH", path, `{"spam_thresholds":{"spam_threshold":0.8}}`, 200)
		got = decode(request("PATCH", path, `{"spam_thresholds":`+clear+`}`, 200))
		if len(got.SpamThresholds) != 0 {
			t.Fatalf("clear %s failed: %v", clear, got.SpamThresholds)
		}
		if len(decode(request("GET", path, "", 200)).SpamThresholds) != 0 {
			t.Fatalf("clear %s did not persist", clear)
		}
	}
}

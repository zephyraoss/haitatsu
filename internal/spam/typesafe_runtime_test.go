package spam

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTypeSafeBudgetSurvivesConfigChanges(t *testing.T) {
	c := NewTypeSafeClient()
	cfg := typeSafeTestConfig("")
	cfg.MaxInFlight = 1
	cfg.MaxRequestsPerMinute = 2
	first, status := c.acquire(cfg)
	if status != "" {
		t.Fatal(status)
	}
	changed := cfg
	changed.Model = "new-model"
	changed.APIKey = "new-key"
	if _, status = c.acquire(changed); status != "skipped_no_capacity" {
		t.Fatal("configuration bypassed concurrency cap", status)
	}
	c.complete(first, "error_auth", 0)
	second, status := c.acquire(changed)
	if status != "" {
		t.Fatal("old error poisoned new configuration", status)
	}
	c.complete(second, "ok", 0)
	changed.MaxInFlight = 3
	if _, status = c.acquire(changed); status != "skipped_rate_limit" {
		t.Fatal("configuration reset rate budget", status)
	}
}

func TestTypeSafeCooldownWaitsThenAllowsOneProbe(t *testing.T) {
	c := NewTypeSafeClient()
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	cfg := typeSafeTestConfig("")
	first, _ := c.acquire(cfg)
	concurrent, _ := c.acquire(cfg)
	c.complete(first, "error_overload", 90*time.Second)
	c.complete(concurrent, "ok", 0)
	now = now.Add(60 * time.Second)
	if _, s := c.acquire(cfg); s != "skipped_cooldown" {
		t.Fatal("premature probe", s)
	}
	now = now.Add(30 * time.Second)
	probe, s := c.acquire(cfg)
	if s != "" || !probe.probe {
		t.Fatal("missing recovery probe", s)
	}
	if _, s := c.acquire(cfg); s != "skipped_cooldown" {
		t.Fatal("concurrent probes allowed", s)
	}
	c.complete(probe, "ok", 0)
	next, s := c.acquire(cfg)
	if s != "" {
		t.Fatal("successful probe did not recover", s)
	}
	c.complete(next, "ok", 0)
}

func TestTypeSafePersistentAuthAndRepeatedFailures(t *testing.T) {
	c := NewTypeSafeClient()
	now := time.Now()
	c.now = func() time.Time { return now }
	cfg := typeSafeTestConfig("")
	lease, _ := c.acquire(cfg)
	c.complete(lease, "error_auth", 0)
	now = now.Add(24 * time.Hour)
	if _, s := c.acquire(cfg); s != "skipped_config" {
		t.Fatal("auth lockout expired without config change", s)
	}
	cfg.APIKey = "rotated"
	for range 3 {
		lease, s := c.acquire(cfg)
		if s != "" {
			t.Fatal(s)
		}
		c.complete(lease, "error_transport", 0)
	}
	if _, s := c.acquire(cfg); s != "skipped_cooldown" {
		t.Fatal("repeated failures did not cool down", s)
	}
}

func TestTypeSafeStrictResponseValidation(t *testing.T) {
	for _, body := range []string{
		`{"answers":{"spam":{"type":"noul","noul":0},"phishing":{"type":"noul","noul":0}}}`,
		`{"model":"m","answers":{"spam":{"type":"noul","noul":0},"phishing":{"type":"noul","noul":0}},"usage":{"input_tokens":-1}}`,
		`{"model":"m","answers":{"spam":{"type":"NOUL","noul":0},"phishing":{"type":"noul","noul":0}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			got := withEndpoint(t, server).Evaluate(context.Background(), typeSafeTestConfig(""), map[string]any{"email": "text"})
			if got.Status != "error_invalid_response" || got.Usage != nil || got.SpamProbability != nil {
				t.Fatalf("accepted invalid response %+v", got)
			}
		})
	}
}

func TestTypeSafeRedirectDoesNotForwardCredentials(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect was followed") }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer server.Close()
	got := withEndpoint(t, server).Evaluate(context.Background(), typeSafeTestConfig(""), map[string]any{"email": "text"})
	if got.Status != "error_http" {
		t.Fatal(got.Status)
	}
}

func TestTypeSafeRetryAfterBounds(t *testing.T) {
	now := time.Now()
	for _, s := range []string{"-1", "0", "junk"} {
		if parseRetryAfterAt(s, now) != 0 {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"9223372036854775807", "999999999999"} {
		if parseRetryAfterAt(s, now) != typeSafeCooldownCap {
			t.Fatal(s)
		}
	}
	if d := parseRetryAfterAt(now.Add(time.Hour).UTC().Format(http.TimeFormat), now); d != typeSafeCooldownCap {
		t.Fatal(d)
	}
	if d := parseRetryAfterAt("3", now); d != 3*time.Second {
		t.Fatal(d)
	}
	// Keep invalid numeric probabilities from ever reaching JSON persistence.
	if validTypeSafeProbability(policyProbability(math.Inf(1))) {
		t.Fatal("infinity accepted")
	}
	if parseRetryAfterAt(strings.Repeat("9", 100), now) != 0 {
		t.Fatal("unparseable integer accepted")
	}
}

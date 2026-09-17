package spam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestTypeSafeClientEndpointOverrideApplies(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"model":"test-model","answers":{"spam":{"type":"noul","noul":0},"phishing":{"type":"noul","noul":0}}}`))
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	cfg := config.TypeSafeConfig{Mode: "junk", APIKey: "k"}.WithDefaults()
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "x"}); result.Status != "ok" {
		t.Fatalf("result: %+v", result)
	}
	if !called {
		t.Fatal("endpoint override was not used")
	}
}

func TestTypeSafeAuthFailureSuppressesRepeatedCalls(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	cfg := config.TypeSafeConfig{Mode: "junk", APIKey: "bad"}.WithDefaults()
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "x"}); result.Status != "error_auth" {
		t.Fatalf("result: %+v", result)
	}
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "x"}); result.Status != "skipped_config" {
		t.Fatalf("second result: %+v", result)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	changed := cfg
	changed.APIKey = "new"
	if result := client.Evaluate(context.Background(), changed, map[string]any{"email": "x"}); result.Status != "error_auth" {
		t.Fatalf("changed key result: %+v", result)
	}
	if calls != 2 {
		t.Fatalf("calls after key change = %d, want 2", calls)
	}
}

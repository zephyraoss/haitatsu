package spam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func typeSafeTestConfig(endpoint string) config.TypeSafeConfig {
	return config.TypeSafeConfig{Mode: "junk", APIKey: "test-key", Model: "test-model", TimeoutMS: 500, MaxTextBytes: 4096, MaxInFlight: 4, MaxRequestsPerMinute: 60}.WithDefaults()
}

// withEndpoint redirects the adapter at a test server without exposing an
// endpoint override in production configuration.
func withEndpoint(t *testing.T, server *httptest.Server) *TypeSafeClient {
	t.Helper()
	client := NewTypeSafeClient()
	client.endpoint = server.URL
	return client
}

func TestTypeSafeClientContract(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-2026-09","answers":{"spam":{"type":"noul","noul":0.99},"phishing":{"type":"noul","noul":0.10}},"usage":{"input_tokens":120,"output_tokens":8}}`))
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	result := client.Evaluate(context.Background(), typeSafeTestConfig(server.URL), map[string]any{"email": "hello"})
	if result.Status != "ok" || result.Model != "jev-2026-09" {
		t.Fatalf("result: %+v", result)
	}
	if result.SpamProbability == nil || *result.SpamProbability != 0.99 || result.PhishingProbability == nil || *result.PhishingProbability != 0.10 {
		t.Fatalf("probabilities: %+v", result)
	}
	if result.Usage == nil || result.Usage.InputTokens != 120 || result.Usage.OutputTokens != 8 {
		t.Fatalf("usage: %+v", result.Usage)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	var request struct {
		Model     string         `json:"model"`
		State     map[string]any `json:"state"`
		Questions map[string]struct {
			Type string `json:"type"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(gotBody), &request); err != nil {
		t.Fatal(err)
	}
	if request.Model != "test-model" || request.State["email"] != "hello" {
		t.Fatalf("request: %+v", request)
	}
	if request.Questions["spam"].Type != "noul" || request.Questions["phishing"].Type != "noul" {
		t.Fatalf("questions: %+v", request.Questions)
	}
}

func TestTypeSafeClientFailuresPreserveFallback(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		status  string
	}{
		{"missing answers", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"m","answers":{"spam":{"type":"noul","noul":0.9}}}`))
		}, "error_invalid_response"},
		{"bad probability", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"test-model","answers":{"spam":{"type":"noul","noul":1.5},"phishing":{"type":"noul","noul":0}}}`))
		}, "error_invalid_response"},
		{"wrong answer type", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"test-model","answers":{"spam":{"type":"logit","noul":0.9},"phishing":{"type":"noul","noul":0}}}`))
		}, "error_invalid_response"},
		{"oversized response", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 64*1024+10)))
		}, "error_oversize_response"},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, "error_auth"},
		{"unprocessable", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}, "error_request_rejected"},
		{"overload", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}, "error_overload"},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, "error_server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			client := withEndpoint(t, server)
			result := client.Evaluate(context.Background(), typeSafeTestConfig(server.URL), map[string]any{"email": "hello"})
			if result.Status != tc.status || result.SpamProbability != nil || result.PhishingProbability != nil {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestTypeSafeClientTimeoutAndStall(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"test-model","answers":`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	client := withEndpoint(t, server)
	cfg := typeSafeTestConfig(server.URL)
	cfg.TimeoutMS = 200
	started := time.Now()
	result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "hello"})
	if result.Status != "error_timeout" && result.Status != "error_transport" && result.Status != "error_invalid_response" {
		t.Fatalf("result: %+v", result)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("timeout did not bound the request")
	}
}

func TestTypeSafeClientDoesNotQueueAndCoolsDown(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	cfg := typeSafeTestConfig(server.URL)
	cfg.MaxRequestsPerMinute = 60
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "one"}); result.Status != "error_overload" {
		t.Fatalf("first result: %+v", result)
	}
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "two"}); result.Status != "skipped_cooldown" {
		t.Fatalf("cooldown result: %+v", result)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestTypeSafeClientRateLimitBudget(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"model":"test-model","answers":{"spam":{"type":"noul","noul":0},"phishing":{"type":"noul","noul":0}}}`))
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	cfg := typeSafeTestConfig(server.URL)
	cfg.MaxRequestsPerMinute = 2
	for range 2 {
		if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "hello"}); result.Status != "ok" {
			t.Fatalf("result: %+v", result)
		}
	}
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "hello"}); result.Status != "skipped_rate_limit" {
		t.Fatalf("over-budget result: %+v", result)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestTypeSafeClientOversizeRequestSkipped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("oversized request reached the provider")
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	result := client.Evaluate(context.Background(), typeSafeTestConfig(server.URL), map[string]any{"email": strings.Repeat("x", typeSafeMaxRequestBytes)})
	if result.Status != "skipped_oversize_request" {
		t.Fatalf("result: %+v", result)
	}
}

func TestTypeSafeClientDisabledMakesNoRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("disabled client made a request")
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	cfg := config.TypeSafeConfig{Mode: "off"}.WithDefaults()
	if result := client.Evaluate(context.Background(), cfg, nil); result.Status != "disabled" {
		t.Fatalf("result: %+v", result)
	}
}

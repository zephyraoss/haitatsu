package spam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestTypeSafeClientConcurrencyCapDoesNotQueue(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 4)
	var (
		mu      sync.Mutex
		started int
		peak    int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		started++
		if started > peak {
			peak = started
		}
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"model":"test-model","answers":{"spam":{"type":"noul","noul":0},"phishing":{"type":"noul","noul":0}}}`))
	}))
	defer server.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	client := withEndpoint(t, server)
	cfg := config.TypeSafeConfig{Mode: "junk", APIKey: "k", MaxInFlight: 2, MaxRequestsPerMinute: 60}.WithDefaults()

	results := make(chan TypeSafeEvaluation, 4)
	for i := range 2 {
		go func(i int) {
			results <- client.Evaluate(context.Background(), cfg, map[string]any{"email": string(rune('a' + i))})
		}(i)
	}
	// Wait for the cap to saturate so the extra call is guaranteed to be refused.
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not start")
		}
	}
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "extra"}); result.Status != "skipped_no_capacity" {
		t.Fatalf("extra call = %+v, want skipped_no_capacity", result)
	}
	unblock()

	okCount := 0
	for range 2 {
		result := <-results
		switch result.Status {
		case "ok":
			okCount++
		case "skipped_no_capacity":
		default:
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if okCount == 0 {
		t.Fatal("no requests completed successfully")
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", peak)
	}
}

func TestTypeSafeClientReloadedLimitsApply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"test-model","answers":{"spam":{"type":"noul","noul":0},"phishing":{"type":"noul","noul":0}}}`))
	}))
	defer server.Close()
	client := withEndpoint(t, server)
	cfg := config.TypeSafeConfig{Mode: "junk", APIKey: "k", MaxRequestsPerMinute: 1}.WithDefaults()
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "a"}); result.Status != "ok" {
		t.Fatalf("first: %+v", result)
	}
	if result := client.Evaluate(context.Background(), cfg, map[string]any{"email": "b"}); result.Status != "skipped_rate_limit" {
		t.Fatalf("second: %+v", result)
	}
	reloaded := cfg
	reloaded.MaxRequestsPerMinute = 10
	if result := client.Evaluate(context.Background(), reloaded, map[string]any{"email": "c"}); result.Status != "ok" {
		t.Fatalf("reloaded: %+v", result)
	}
}

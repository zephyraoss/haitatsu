package logging

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func TestAxiomHandlerKeepsAttrsAndFlushesOnClose(t *testing.T) {
	received := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		received <- batch
	}))
	defer server.Close()

	logger, closeLog, err := New(config.LoggingConfig{AxiomEnabled: true, AxiomURL: server.URL, AxiomDataset: "test", AxiomToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	logger.With("version", "v1").WithGroup("req").Info("hello", "id", "abc")
	closeLog()

	select {
	case batch := <-received:
		if len(batch) != 1 || batch[0]["version"] != "v1" || batch[0]["req.id"] != "abc" || batch[0]["message"] != "hello" {
			t.Fatalf("batch = %#v", batch)
		}
	default:
		t.Fatal("expected batch to be flushed on close")
	}
}

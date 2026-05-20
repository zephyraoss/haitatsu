package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func New(cfg config.LoggingConfig) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	handler := slog.Handler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if cfg.AxiomEnabled {
		handler = teeHandler{primary: handler, secondary: newAxiomHandler(cfg, level)}
	}
	return slog.New(handler), nil
}

type teeHandler struct {
	primary   slog.Handler
	secondary slog.Handler
}

func (h teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level) || h.secondary.Enabled(ctx, level)
}

func (h teeHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := h.primary.Handle(ctx, record); err != nil {
		return err
	}
	return h.secondary.Handle(ctx, record)
}

func (h teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{primary: h.primary.WithAttrs(attrs), secondary: h.secondary.WithAttrs(attrs)}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{primary: h.primary.WithGroup(name), secondary: h.secondary.WithGroup(name)}
}

type axiomHandler struct {
	level  slog.Level
	url    string
	token  string
	client *http.Client
	queue  chan map[string]any
}

func newAxiomHandler(cfg config.LoggingConfig, level slog.Level) *axiomHandler {
	baseURL := strings.TrimRight(cfg.AxiomURL, "/")
	if baseURL == "" {
		baseURL = "https://api.axiom.co"
	}
	handler := &axiomHandler{
		level:  level,
		url:    baseURL + "/v1/datasets/" + cfg.AxiomDataset + "/ingest",
		token:  cfg.AxiomToken,
		client: &http.Client{Timeout: 5 * time.Second},
		queue:  make(chan map[string]any, 1000),
	}
	go handler.flushLoop()
	return handler
}

func (h *axiomHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *axiomHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level < h.level {
		return nil
	}
	entry := map[string]any{"_time": record.Time, "level": record.Level.String(), "message": record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		entry[attr.Key] = attr.Value.Any()
		return true
	})
	select {
	case h.queue <- entry:
	case <-ctx.Done():
	default:
	}
	return nil
}

func (h *axiomHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *axiomHandler) WithGroup(string) slog.Handler      { return h }

func (h *axiomHandler) flushLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	batch := make([]map[string]any, 0, 100)
	for {
		select {
		case entry := <-h.queue:
			batch = append(batch, entry)
			if len(batch) >= 100 {
				h.send(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				h.send(batch)
				batch = batch[:0]
			}
		}
	}
}

func (h *axiomHandler) send(batch []map[string]any) {
	body, err := json.Marshal(batch)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level %q", value)
	}
}

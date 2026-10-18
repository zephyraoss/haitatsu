package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func New(cfg config.LoggingConfig) (*slog.Logger, func(), error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}

	handler := slog.Handler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	closer := func() {}
	if cfg.AxiomEnabled {
		axiom := newAxiomHandler(cfg, level)
		handler = teeHandler{primary: handler, secondary: axiom}
		closer = axiom.sink.close
	}
	return slog.New(handler), closer, nil
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
	sink   *axiomSink
	attrs  map[string]any
	prefix string
}

type axiomSink struct {
	url      string
	token    string
	client   *http.Client
	queue    chan map[string]any
	done     chan struct{}
	finished chan struct{}
	once     sync.Once
}

func newAxiomHandler(cfg config.LoggingConfig, level slog.Level) axiomHandler {
	baseURL := strings.TrimRight(cfg.AxiomURL, "/")
	if baseURL == "" {
		baseURL = "https://api.axiom.co"
	}
	sink := &axiomSink{
		url:      baseURL + "/v1/datasets/" + cfg.AxiomDataset + "/ingest",
		token:    cfg.AxiomToken,
		client:   &http.Client{Timeout: 5 * time.Second},
		queue:    make(chan map[string]any, 1000),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	go sink.flushLoop()
	return axiomHandler{level: level, sink: sink}
}

func (h axiomHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h axiomHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level < h.level {
		return nil
	}
	entry := map[string]any{"_time": record.Time, "level": record.Level.String(), "message": record.Message}
	for key, value := range h.attrs {
		entry[key] = value
	}
	record.Attrs(func(attr slog.Attr) bool {
		entry[h.prefix+attr.Key] = attr.Value.Any()
		return true
	})
	select {
	case <-h.sink.done:
		return nil
	default:
	}
	select {
	case h.sink.queue <- entry:
	case <-ctx.Done():
	default:
	}
	return nil
}

func (h axiomHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := maps.Clone(h.attrs)
	if next == nil {
		next = make(map[string]any, len(attrs))
	}
	for _, attr := range attrs {
		next[h.prefix+attr.Key] = attr.Value.Any()
	}
	h.attrs = next
	return h
}

func (h axiomHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h.prefix += name + "."
	return h
}

func (s *axiomSink) close() {
	s.once.Do(func() { close(s.done) })
	<-s.finished
}

func (s *axiomSink) flushLoop() {
	defer close(s.finished)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	batch := make([]map[string]any, 0, 100)
	for {
		select {
		case entry := <-s.queue:
			batch = append(batch, entry)
			if len(batch) >= 100 {
				s.send(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.send(batch)
				batch = batch[:0]
			}
		case <-s.done:
			for {
				select {
				case entry := <-s.queue:
					batch = append(batch, entry)
					if len(batch) >= 100 {
						s.send(batch)
						batch = batch[:0]
					}
				default:
					if len(batch) > 0 {
						s.send(batch)
					}
					return
				}
			}
		}
	}
}

func (s *axiomSink) send(batch []map[string]any) {
	body, err := json.Marshal(batch)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
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

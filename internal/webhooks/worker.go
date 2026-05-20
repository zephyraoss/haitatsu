package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
)

const maxAttempts = 3

type Worker struct {
	db       *sql.DB
	client   *ent.Client
	cfg      config.WebhookConfig
	workerID string
	http     *http.Client
}

type eventJob struct {
	ID        string
	EventType string
	TraceID   string
	Payload   map[string]any
	Attempts  int
}

func NewWorker(db *sql.DB, client *ent.Client, cfg config.WebhookConfig, workerID string) *Worker {
	timeout := time.Duration(cfg.DefaultTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Worker{db: db, client: client, cfg: cfg, workerID: workerID, http: &http.Client{Timeout: timeout}}
}

func (w *Worker) Run(ctx context.Context, concurrency int) {
	if concurrency <= 0 {
		concurrency = 1
	}
	for range concurrency {
		go w.loop(ctx)
	}
}

func (w *Worker) loop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.processOne(ctx)
		}
	}
}

func (w *Worker) processOne(ctx context.Context) error {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return err
	}
	endpoint := w.cfg.Endpoints[job.EventType]
	if endpoint == "" {
		_, err := w.client.EventLog.UpdateOneID(job.ID).SetStatus("no_endpoint").ClearLockedBy().ClearLockedUntil().Save(ctx)
		return err
	}
	body, err := json.Marshal(job.Payload)
	if err != nil {
		return w.fail(ctx, job, err)
	}
	if err := w.post(ctx, endpoint, job, body); err != nil {
		return w.fail(ctx, job, err)
	}
	_, err = w.client.EventLog.UpdateOneID(job.ID).SetStatus("delivered").ClearLockedBy().ClearLockedUntil().ClearNextAttemptAt().Save(ctx)
	return err
}

func (w *Worker) claim(ctx context.Context) (eventJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return eventJob{}, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
UPDATE event_logs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM event_logs
  WHERE status IN ('queued', 'retry')
    AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
    AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, event_type, trace_id, payload, attempts
`, w.workerID, time.Now().Add(5*time.Minute))

	var job eventJob
	var payload []byte
	if err := row.Scan(&job.ID, &job.EventType, &job.TraceID, &payload, &job.Attempts); err != nil {
		if err == sql.ErrNoRows {
			return eventJob{}, false, nil
		}
		return eventJob{}, false, err
	}
	if err := json.Unmarshal(payload, &job.Payload); err != nil {
		return eventJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return eventJob{}, false, err
	}
	return job, true, nil
}

func (w *Worker) post(ctx context.Context, endpoint string, job eventJob, body []byte) error {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Haitatsu-Event", job.EventType)
	req.Header.Set("X-Haitatsu-Timestamp", timestamp)
	req.Header.Set("X-Haitatsu-Signature", signature(w.cfg.Secret, timestamp, body))
	req.Header.Set("X-Haitatsu-Trace-ID", job.TraceID)

	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

func (w *Worker) fail(ctx context.Context, job eventJob, err error) error {
	update := w.client.EventLog.UpdateOneID(job.ID).ClearLockedBy().ClearLockedUntil().AddAttempts(1).SetLastError(map[string]any{"error": err.Error()})
	if job.Attempts+1 >= maxAttempts {
		update.SetStatus("failed")
	} else {
		update.SetStatus("retry").SetNextAttemptAt(time.Now().Add(time.Duration(job.Attempts+1) * time.Minute))
	}
	_, saveErr := update.Save(ctx)
	return saveErr
}

func signature(secret string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

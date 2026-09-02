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
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

type Worker struct {
	db       *sql.DB
	client   *ent.Client
	cfg      func() config.WebhookConfig
	metrics  *metrics.Metrics
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

func NewWorker(db *sql.DB, client *ent.Client, cfg func() config.WebhookConfig, metrics *metrics.Metrics, workerID string) *Worker {
	return &Worker{db: db, client: client, cfg: cfg, metrics: metrics, workerID: workerID, http: &http.Client{}}
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
			for {
				processed, err := w.ProcessOne(ctx)
				if err != nil || !processed || ctx.Err() != nil {
					break
				}
			}
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	cfg := w.cfg()
	endpoint := cfg.Endpoints[job.EventType]
	if endpoint == "" {
		_, err := w.client.EventLog.UpdateOneID(job.ID).SetStatus("no_endpoint").ClearLockedBy().ClearLockedUntil().Save(ctx)
		return true, err
	}
	body, err := json.Marshal(job.Payload)
	if err != nil {
		return true, w.fail(ctx, cfg, job, err)
	}
	if err := w.post(ctx, cfg, endpoint, job, body); err != nil {
		w.metrics.WebhookFailure()
		return true, w.fail(ctx, cfg, job, err)
	}
	w.metrics.WorkerJob("webhook", "delivered")
	_, err = w.client.EventLog.UpdateOneID(job.ID).SetStatus("delivered").ClearLockedBy().ClearLockedUntil().ClearNextAttemptAt().Save(ctx)
	return true, err
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

func (w *Worker) post(ctx context.Context, cfg config.WebhookConfig, endpoint string, job eventJob, body []byte) error {
	timeout := time.Duration(cfg.DefaultTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timestamp := time.Now().UTC().Format(time.RFC3339)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Haitatsu-Event", job.EventType)
	req.Header.Set("X-Haitatsu-Timestamp", timestamp)
	req.Header.Set("X-Haitatsu-Signature", signature(cfg.Secret, timestamp, body))
	req.Header.Set("X-Haitatsu-Trace-ID", job.TraceID)
	req.Header.Set("X-Haitatsu-Delivery", job.ID)

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

func (w *Worker) fail(ctx context.Context, cfg config.WebhookConfig, job eventJob, err error) error {
	policy := cfg.RetryPolicy()
	update := w.client.EventLog.UpdateOneID(job.ID).ClearLockedBy().ClearLockedUntil().AddAttempts(1).SetLastError(map[string]any{"error": err.Error()})
	if policy.Exhausted(job.Attempts + 1) {
		update.SetStatus("failed")
	} else {
		update.SetStatus("retry").SetNextAttemptAt(time.Now().Add(policy.Delay(job.Attempts)))
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

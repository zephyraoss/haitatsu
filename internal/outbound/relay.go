package outbound

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

type RelayStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
}

type Notifier interface {
	OutboundFailure(ctx context.Context, mailboxID string, messageID string, data map[string]any) error
}

type Sender func(ctx context.Context, cfg config.RelayConfig, from string, recipients []string, raw []byte) error

type Worker struct {
	db       *sql.DB
	client   *ent.Client
	store    RelayStore
	cfg      func() config.RelayConfig
	metrics  *metrics.Metrics
	notifier Notifier
	workerID string
	backend  database.Backend
	send     Sender
}

type claimedJob struct {
	ID         string
	MailboxID  string
	MessageID  string
	ReturnPath string
	Recipients []string
	Attempts   int
}

type deliveryResult struct {
	Code               int
	EnhancedStatusCode string
	Classification     string
	Response           string
}

func NewWorker(db *sql.DB, client *ent.Client, store RelayStore, cfg func() config.RelayConfig, metrics *metrics.Metrics, notifier Notifier, workerID string, backends ...database.Backend) *Worker {
	backend := database.BackendPostgres
	if len(backends) > 0 {
		backend = backends[0]
	}
	return &Worker{db: db, client: client, store: store, cfg: cfg, metrics: metrics, notifier: notifier, workerID: workerID, backend: backend, send: sendViaRelay}
}

func (w *Worker) WithSender(send Sender) *Worker {
	w.send = send
	return w
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
	result := w.deliver(ctx, job)
	w.metrics.WorkerJob("outbound", result.Classification)
	if result.Classification == "success" {
		w.metrics.MessageSent()
	}
	if _, err := w.client.OutboundAttempt.Create().
		SetOutboundJobID(job.ID).
		SetMessageID(job.MessageID).
		SetClassification(result.Classification).
		SetResponse(result.Response).
		SetNillableSMTPCode(codePtr(result.Code)).
		SetEnhancedStatusCode(result.EnhancedStatusCode).
		Save(ctx); err != nil {
		return true, err
	}
	return true, w.finish(ctx, job, result)
}

func (w *Worker) claim(ctx context.Context) (claimedJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return claimedJob{}, false, err
	}
	defer tx.Rollback()

	query := `
UPDATE outbound_jobs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM outbound_jobs
  WHERE status IN ('queued', 'retry')
    AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
    AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id, message_id, return_path, recipients, attempts
`
	now := time.Now().UTC()
	args := []any{w.workerID, now.Add(5 * time.Minute)}
	if w.backend.SQLiteFamily() {
		query = `
UPDATE outbound_jobs SET locked_by = ?, locked_until = ?, status = 'processing', updated_at = ?
WHERE id = (
  SELECT id FROM outbound_jobs
  WHERE status IN ('queued', 'retry')
    AND (next_attempt_at IS NULL OR datetime(next_attempt_at) <= datetime(?))
    AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
  ORDER BY created_at
  LIMIT 1
)
AND status IN ('queued', 'retry')
AND (next_attempt_at IS NULL OR datetime(next_attempt_at) <= datetime(?))
AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
RETURNING id, mailbox_id, message_id, return_path, recipients, attempts
`
		args = []any{w.workerID, now.Add(5 * time.Minute), now, now, now, now, now}
	}
	row := tx.QueryRowContext(ctx, query, args...)

	var job claimedJob
	var recipients []byte
	if err := row.Scan(&job.ID, &job.MailboxID, &job.MessageID, &job.ReturnPath, &recipients, &job.Attempts); err != nil {
		if err == sql.ErrNoRows {
			return claimedJob{}, false, nil
		}
		return claimedJob{}, false, err
	}
	if err := json.Unmarshal(recipients, &job.Recipients); err != nil {
		return claimedJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return claimedJob{}, false, err
	}
	return job, true, nil
}

func (w *Worker) deliver(ctx context.Context, job claimedJob) deliveryResult {
	msg, err := w.client.Message.Query().Where(message.IDEQ(job.MessageID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return permanentFailure(550, "5.1.0", "message no longer exists")
		}
		return localFailure(err)
	}
	raw, err := w.store.GetMessage(ctx, msg.BlobKey)
	if err != nil {
		return localFailure(err)
	}
	from := job.ReturnPath
	if from == "" && len(msg.FromAddresses) > 0 {
		from = msg.FromAddresses[0]
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return permanentFailure(553, "5.1.7", err.Error())
	}
	if len(job.Recipients) == 0 {
		return permanentFailure(553, "5.1.3", "no recipients")
	}

	err = w.send(ctx, w.cfg(), from, job.Recipients, raw)
	if err == nil {
		return deliveryResult{Code: 250, Classification: "success", Response: "sent"}
	}
	return classifySMTPError(err)
}

func sendViaRelay(_ context.Context, cfg config.RelayConfig, from string, recipients []string, raw []byte) error {
	return smtp.SendMail(cfg.Addr, relayAuth(cfg), from, recipients, raw)
}

func (w *Worker) finish(ctx context.Context, job claimedJob, result deliveryResult) error {
	policy := w.cfg().RetryPolicy()
	update := w.client.OutboundJob.UpdateOneID(job.ID).ClearLockedBy().ClearLockedUntil()
	failed := false
	switch result.Classification {
	case "success":
		update.SetStatus("sent").ClearNextAttemptAt()
	case "temporary":
		if policy.Exhausted(job.Attempts + 1) {
			update.SetStatus("failed")
			failed = true
		} else {
			update.SetStatus("retry").SetNextAttemptAt(time.Now().Add(policy.Delay(job.Attempts)))
		}
		update.AddAttempts(1).SetLastError(result.errorMap())
	default:
		update.SetStatus("failed").AddAttempts(1).SetLastError(result.errorMap())
		failed = true
	}
	_, err := update.Save(ctx)
	if err == nil && failed && w.notifier != nil {
		_ = w.notifier.OutboundFailure(ctx, job.MailboxID, job.MessageID, result.errorMap())
	}
	return err
}

func relayAuth(cfg config.RelayConfig) smtp.Auth {
	if cfg.Username == "" {
		return nil
	}
	host := cfg.FromHost
	if host == "" {
		host, _, _ = strings.Cut(cfg.Addr, ":")
	}
	return smtp.PlainAuth("", cfg.Username, cfg.Password, host)
}

func classifySMTPError(err error) deliveryResult {
	message := err.Error()
	code := smtpCode(message)
	enhanced := enhancedStatusCode(message)
	if code >= 500 {
		return permanentFailure(code, enhanced, message)
	}
	return deliveryResult{Code: code, EnhancedStatusCode: enhanced, Classification: "temporary", Response: message}
}

func localFailure(err error) deliveryResult {
	return deliveryResult{Classification: "temporary", Response: err.Error()}
}

func permanentFailure(code int, enhanced string, response string) deliveryResult {
	return deliveryResult{Code: code, EnhancedStatusCode: enhanced, Classification: "permanent", Response: response}
}

var smtpCodePattern = regexp.MustCompile(`\b([245][0-9]{2})\b`)
var enhancedCodePattern = regexp.MustCompile(`\b[245]\.\d+\.\d+\b`)

func smtpCode(message string) int {
	match := smtpCodePattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	code, _ := strconv.Atoi(match[1])
	return code
}

func enhancedStatusCode(message string) string {
	return enhancedCodePattern.FindString(message)
}

func codePtr(code int) *int {
	if code == 0 {
		return nil
	}
	return &code
}

func (r deliveryResult) errorMap() map[string]any {
	return map[string]any{"code": r.Code, "enhanced_status_code": r.EnhancedStatusCode, "classification": r.Classification, "response": r.Response}
}

func (j claimedJob) String() string {
	return fmt.Sprintf("%s/%s", j.ID, j.MessageID)
}

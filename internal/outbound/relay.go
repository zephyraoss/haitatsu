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
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
)

const maxRelayAttempts = 5

type RelayStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
}

type Worker struct {
	db       *sql.DB
	client   *ent.Client
	store    RelayStore
	cfg      config.RelayConfig
	workerID string
}

type claimedJob struct {
	ID         string
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

func NewWorker(db *sql.DB, client *ent.Client, store RelayStore, cfg config.RelayConfig, workerID string) *Worker {
	return &Worker{db: db, client: client, store: store, cfg: cfg, workerID: workerID}
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
	result := w.deliver(ctx, job)
	if _, err := w.client.OutboundAttempt.Create().
		SetOutboundJobID(job.ID).
		SetMessageID(job.MessageID).
		SetClassification(result.Classification).
		SetResponse(result.Response).
		SetNillableSMTPCode(codePtr(result.Code)).
		SetEnhancedStatusCode(result.EnhancedStatusCode).
		Save(ctx); err != nil {
		return err
	}
	return w.finish(ctx, job, result)
}

func (w *Worker) claim(ctx context.Context) (claimedJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return claimedJob{}, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
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
RETURNING id, message_id, return_path, recipients, attempts
`, w.workerID, time.Now().Add(5*time.Minute))

	var job claimedJob
	var recipients []byte
	if err := row.Scan(&job.ID, &job.MessageID, &job.ReturnPath, &recipients, &job.Attempts); err != nil {
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
		return permanentFailure(553, "", err.Error())
	}
	if len(job.Recipients) == 0 {
		return permanentFailure(553, "", "no recipients")
	}

	err = smtp.SendMail(w.cfg.Addr, w.auth(), from, job.Recipients, raw)
	if err == nil {
		return deliveryResult{Code: 250, Classification: "success", Response: "sent"}
	}
	return classifySMTPError(err)
}

func (w *Worker) finish(ctx context.Context, job claimedJob, result deliveryResult) error {
	update := w.client.OutboundJob.UpdateOneID(job.ID).ClearLockedBy().ClearLockedUntil()
	switch result.Classification {
	case "success":
		update.SetStatus("sent").ClearNextAttemptAt()
	case "temporary":
		if job.Attempts+1 >= maxRelayAttempts {
			update.SetStatus("failed")
		} else {
			update.SetStatus("retry").SetNextAttemptAt(time.Now().Add(time.Duration(job.Attempts+1) * time.Minute))
		}
		update.AddAttempts(1).SetLastError(result.errorMap())
	default:
		update.SetStatus("failed").AddAttempts(1).SetLastError(result.errorMap())
	}
	_, err := update.Save(ctx)
	return err
}

func (w *Worker) auth() smtp.Auth {
	if w.cfg.Username == "" {
		return nil
	}
	host := w.cfg.FromHost
	if host == "" {
		host, _, _ = strings.Cut(w.cfg.Addr, ":")
	}
	return smtp.PlainAuth("", w.cfg.Username, w.cfg.Password, host)
}

func classifySMTPError(err error) deliveryResult {
	message := err.Error()
	code := smtpCode(message)
	enhanced := enhancedStatusCode(message)
	if code >= 500 {
		return permanentFailure(code, enhanced, message)
	}
	if code >= 400 {
		return deliveryResult{Code: code, EnhancedStatusCode: enhanced, Classification: "temporary", Response: message}
	}
	return deliveryResult{Code: code, EnhancedStatusCode: enhanced, Classification: "temporary", Response: message}
}

func localFailure(err error) deliveryResult {
	return deliveryResult{Classification: "temporary", Response: err.Error()}
}

func permanentFailure(code int, enhanced string, response string) deliveryResult {
	return deliveryResult{Code: code, EnhancedStatusCode: enhanced, Classification: "permanent", Response: response}
}

func smtpCode(message string) int {
	match := regexp.MustCompile(`\b([245][0-9]{2})\b`).FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	code, _ := strconv.Atoi(match[1])
	return code
}

func enhancedStatusCode(message string) string {
	match := regexp.MustCompile(`\b[245]\.\d+\.\d+\b`).FindString(message)
	return match
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

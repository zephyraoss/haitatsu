package outbound

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/database/ent/outboundjob"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

type RelayStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
}

type Notifier interface {
	OutboundFailure(ctx context.Context, mailboxID string, messageID string, data map[string]any) error
}

type EventEmitter interface {
	Emit(ctx context.Context, eventType string, mailboxID string, payload map[string]any) error
}

type Sender func(ctx context.Context, cfg config.RelayConfig, from string, recipients []string, raw []byte) error

type Worker struct {
	db       *sql.DB
	client   *ent.Client
	store    RelayStore
	cfg      func() config.RelayConfig
	metrics  *metrics.Metrics
	notifier Notifier
	events   EventEmitter
	workerID string
	backend  database.Backend
	send     Sender
	hostname string
}

type claimedJob struct {
	ID           string
	MailboxID    string
	SenderDomain string
	MessageID    string
	ReturnPath   string
	Recipients   []string
	Attempts     int
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
	w := &Worker{db: db, client: client, store: store, cfg: cfg, metrics: metrics, notifier: notifier, workerID: workerID, backend: backend, hostname: "localhost"}
	w.send = w.sendViaRelay
	return w
}

func (w *Worker) WithHostname(hostname string) *Worker {
	w.hostname = hostname
	return w
}

func (w *Worker) WithEvents(events EventEmitter) *Worker {
	w.events = events
	return w
}

func (w *Worker) WithSender(send Sender) *Worker {
	w.send = send
	return w
}

func (w *Worker) Run(ctx context.Context, concurrency int) {
	if concurrency <= 0 {
		concurrency = 1
	}
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() { w.loop(ctx) })
	}
	wg.Wait()
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	w.metrics.WorkerJob("outbound", result.Classification)
	if result.Classification == "success" {
		w.metrics.MessageSent()
	}
	_, attemptErr := w.client.OutboundAttempt.Create().
		SetOutboundJobID(job.ID).
		SetMessageID(job.MessageID).
		SetClassification(result.Classification).
		SetResponse(result.Response).
		SetNillableSMTPCode(codePtr(result.Code)).
		SetEnhancedStatusCode(result.EnhancedStatusCode).
		Save(ctx)
	return true, errors.Join(attemptErr, w.finish(ctx, job, result))
}

func (w *Worker) claim(ctx context.Context) (claimedJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return claimedJob{}, false, err
	}
	defer tx.Rollback()

	query := `
UPDATE outbound_jobs SET locked_by = $1, locked_until = $2, attempts = attempts + CASE WHEN status = 'processing' THEN 1 ELSE 0 END, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM outbound_jobs
  WHERE status IN ('queued', 'retry', 'processing')
    AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
    AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id, sender_domain, message_id, return_path, recipients, attempts
`
	now := time.Now().UTC()
	lockedUntil := now.Add(w.cfg().Timeout() + 4*time.Minute)
	args := []any{w.workerID, lockedUntil}
	if w.backend.SQLiteFamily() {
		query = `
UPDATE outbound_jobs SET locked_by = ?, locked_until = ?, attempts = attempts + CASE WHEN status = 'processing' THEN 1 ELSE 0 END, status = 'processing', updated_at = ?
WHERE id = (
  SELECT id FROM outbound_jobs
  WHERE status IN ('queued', 'retry', 'processing')
    AND (next_attempt_at IS NULL OR datetime(next_attempt_at) <= datetime(?))
    AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
  ORDER BY created_at
  LIMIT 1
)
AND status IN ('queued', 'retry', 'processing')
AND (next_attempt_at IS NULL OR datetime(next_attempt_at) <= datetime(?))
AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
RETURNING id, mailbox_id, sender_domain, message_id, return_path, recipients, attempts
`
		args = []any{w.workerID, lockedUntil, now, now, now, now, now}
	}
	row := tx.QueryRowContext(ctx, query, args...)

	var job claimedJob
	var recipients []byte
	if err := row.Scan(&job.ID, &job.MailboxID, &job.SenderDomain, &job.MessageID, &job.ReturnPath, &recipients, &job.Attempts); err != nil {
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

func (w *Worker) sendViaRelay(ctx context.Context, cfg config.RelayConfig, from string, recipients []string, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout())
	defer cancel()

	deadline, _ := ctx.Deadline()
	dialer := &net.Dialer{Timeout: cfg.Timeout()}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	host, _, _ := strings.Cut(cfg.Addr, ":")
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return wrapCtxErr(ctx, err)
	}
	defer client.Close()
	if err := client.Hello(w.hostname); err != nil {
		return wrapCtxErr(ctx, err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return wrapCtxErr(ctx, err)
		}
	}
	if auth := relayAuth(cfg); auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err := client.Auth(auth); err != nil {
			return wrapCtxErr(ctx, err)
		}
	}
	if err := client.Mail(from); err != nil {
		return wrapCtxErr(ctx, err)
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return wrapCtxErr(ctx, err)
		}
	}
	wc, err := client.Data()
	if err != nil {
		return wrapCtxErr(ctx, err)
	}
	if _, err := wc.Write(raw); err != nil {
		return wrapCtxErr(ctx, err)
	}
	if err := wc.Close(); err != nil {
		return wrapCtxErr(ctx, err)
	}
	_ = client.Quit()
	return nil
}

func wrapCtxErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %v", ctx.Err(), err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", context.DeadlineExceeded, err)
	}
	return err
}

func (w *Worker) finish(ctx context.Context, job claimedJob, result deliveryResult) error {
	policy := w.cfg().RetryPolicy()
	update := w.client.OutboundJob.UpdateOneID(job.ID).Where(outboundjob.LockedByEQ(w.workerID)).ClearLockedBy().ClearLockedUntil()
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
	if err == nil && failed {
		w.reportFailure(ctx, job, result)
	}
	return err
}

func (w *Worker) reportFailure(ctx context.Context, job claimedJob, result deliveryResult) {
	if job.SenderDomain == "" {
		if w.notifier != nil {
			_ = w.notifier.OutboundFailure(ctx, job.MailboxID, job.MessageID, result.errorMap())
		}
		return
	}
	if w.events != nil {
		_ = w.events.Emit(ctx, events.MessageBounced, "", map[string]any{
			"event":         events.MessageBounced,
			"source":        "relay",
			"message_id":    job.MessageID,
			"sender_domain": job.SenderDomain,
			"recipients":    job.Recipients,
			"error":         result.errorMap(),
		})
	}
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
	var smtpErr *textproto.Error
	if !errors.As(err, &smtpErr) {
		return deliveryResult{Classification: "temporary", Response: message}
	}
	enhanced := enhancedStatusCode(smtpErr.Msg)
	if smtpErr.Code >= 500 {
		return permanentFailure(smtpErr.Code, enhanced, message)
	}
	return deliveryResult{Code: smtpErr.Code, EnhancedStatusCode: enhanced, Classification: "temporary", Response: message}
}

func localFailure(err error) deliveryResult {
	return deliveryResult{Classification: "temporary", Response: err.Error()}
}

func permanentFailure(code int, enhanced string, response string) deliveryResult {
	return deliveryResult{Code: code, EnhancedStatusCode: enhanced, Classification: "permanent", Response: response}
}

var enhancedCodePattern = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}\b`)

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

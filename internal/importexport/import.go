package importexport

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	messagepred "github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

type ImportWorker struct {
	db       *sql.DB
	client   *ent.Client
	store    Store
	events   *events.Service
	workerID string
}

type importJob struct {
	ID         string
	MailboxID  string
	SourceType string
	Source     map[string]any
}

func NewImportWorker(db *sql.DB, client *ent.Client, store Store, events *events.Service, workerID string) *ImportWorker {
	return &ImportWorker{db: db, client: client, store: store, events: events, workerID: workerID}
}

func (w *ImportWorker) Run(ctx context.Context, concurrency int) {
	if concurrency <= 0 {
		concurrency = 1
	}
	for range concurrency {
		go w.loop(ctx)
	}
}

func (w *ImportWorker) loop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
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

func (w *ImportWorker) processOne(ctx context.Context) error {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return err
	}
	count, err := w.importJob(ctx, job)
	if err != nil {
		return w.fail(ctx, job, err)
	}
	_, err = w.client.ImportJob.UpdateOneID(job.ID).SetStatus("completed").SetImportedCount(count).ClearLockedBy().ClearLockedUntil().Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxImportCompleted, job.MailboxID, map[string]any{"event": events.MailboxImportCompleted, "import_id": job.ID, "mailbox_id": job.MailboxID, "imported_count": count})
}

func (w *ImportWorker) claim(ctx context.Context) (importJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return importJob{}, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
UPDATE import_jobs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM import_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id, source_type, source
`, w.workerID, time.Now().Add(10*time.Minute))

	var job importJob
	var source []byte
	if err := row.Scan(&job.ID, &job.MailboxID, &job.SourceType, &source); err != nil {
		if err == sql.ErrNoRows {
			return importJob{}, false, nil
		}
		return importJob{}, false, err
	}
	if err := json.Unmarshal(source, &job.Source); err != nil {
		return importJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return importJob{}, false, err
	}
	return job, true, nil
}

func (w *ImportWorker) importJob(ctx context.Context, job importJob) (int, error) {
	if job.SourceType != "zip" {
		return 0, fmt.Errorf("unsupported import source type %q", job.SourceType)
	}
	key := sourceKey(job.Source)
	if key == "" {
		return 0, fmt.Errorf("zip import requires source.object_key")
	}
	data, err := w.store.GetObject(ctx, key)
	if err != nil {
		return 0, err
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, err
	}
	mbox, err := w.client.Mailbox.Query().Where(mailbox.IDEQ(job.MailboxID)).Only(ctx)
	if err != nil {
		return 0, err
	}
	inbox, err := w.client.Folder.Query().Where(folder.MailboxIDEQ(job.MailboxID), folder.NameEQ("INBOX")).Only(ctx)
	if err != nil {
		return 0, err
	}

	imported := 0
	for _, file := range archive.File {
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".eml") {
			continue
		}
		raw, err := readZipFile(file)
		if err != nil {
			return imported, err
		}
		msg, err := w.messageForRaw(ctx, raw)
		if err != nil {
			return imported, err
		}
		_, err = w.client.MailboxMessage.Create().
			SetMailboxID(job.MailboxID).
			SetMessageID(msg.ID).
			SetFolderID(inbox.ID).
			SetOriginalRcpt(mbox.PrimaryAddress).
			SetBaseRcpt(mbox.PrimaryAddress).
			Save(ctx)
		if ent.IsConstraintError(err) {
			continue
		}
		if err != nil {
			return imported, err
		}
		if _, err := w.client.Mailbox.UpdateOneID(job.MailboxID).AddUsedBytes(int64(len(raw))).Save(ctx); err != nil {
			return imported, err
		}
		imported++
	}
	return imported, nil
}

func (w *ImportWorker) messageForRaw(ctx context.Context, raw []byte) (*ent.Message, error) {
	sha := sha256Hex(raw)
	existing, err := w.client.Message.Query().Where(messagepred.Sha256EQ(sha)).First(ctx)
	if err == nil {
		return existing, nil
	}
	if err != nil && !ent.IsNotFound(err) {
		return nil, err
	}

	messageID := ids.New().String()
	traceID := ids.New().String()
	key := messageObjectKey(time.Now().UTC(), messageID)
	if err := w.store.PutMessage(ctx, key, raw); err != nil {
		return nil, err
	}
	metadata := mailparse.Parse(raw)
	create := w.client.Message.Create().
		SetID(messageID).
		SetTraceID(traceID).
		SetBlobKey(key).
		SetSha256(sha).
		SetSizeBytes(int64(len(raw))).
		SetHeaders(metadata.Headers).
		SetFromAddresses(metadata.From).
		SetToAddresses(metadata.To).
		SetCcAddresses(metadata.CC).
		SetBccAddresses(metadata.BCC).
		SetSubject(metadata.Subject).
		SetTextBodyExtract(metadata.TextExtract).
		SetHTMLBodyExtract(metadata.HTMLExtract).
		SetAttachments(metadata.Attachments).
		SetAuthResults(map[string]any{})
	if metadata.RFCMessageID != "" {
		create.SetRfcMessageID(metadata.RFCMessageID)
	}
	if metadata.Date != nil {
		create.SetDate(*metadata.Date)
	}
	return create.Save(ctx)
}

func (w *ImportWorker) fail(ctx context.Context, job importJob, cause error) error {
	_, err := w.client.ImportJob.UpdateOneID(job.ID).SetStatus("failed").SetLastError(map[string]any{"error": cause.Error()}).ClearLockedBy().ClearLockedUntil().Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxImportFailed, job.MailboxID, map[string]any{"event": events.MailboxImportFailed, "import_id": job.ID, "mailbox_id": job.MailboxID, "error": cause.Error()})
}

func sourceKey(source map[string]any) string {
	if value, _ := source["object_key"].(string); value != "" {
		return value
	}
	value, _ := source["key"].(string)
	return value
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func messageObjectKey(t time.Time, messageID string) string {
	return fmt.Sprintf("messages/%04d/%02d/%02d/%s.eml", t.Year(), t.Month(), t.Day(), messageID)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

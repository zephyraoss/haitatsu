package importexport

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/events"
)

type Store interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
	PutExport(ctx context.Context, key string, data []byte) error
}

type ExportWorker struct {
	db       *sql.DB
	client   *ent.Client
	store    Store
	events   *events.Service
	workerID string
}

type exportJob struct {
	ID        string
	MailboxID string
}

func NewExportWorker(db *sql.DB, client *ent.Client, store Store, events *events.Service, workerID string) *ExportWorker {
	return &ExportWorker{db: db, client: client, store: store, events: events, workerID: workerID}
}

func (w *ExportWorker) Run(ctx context.Context, concurrency int) {
	if concurrency <= 0 {
		concurrency = 1
	}
	for range concurrency {
		go w.loop(ctx)
	}
}

func (w *ExportWorker) loop(ctx context.Context) {
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

func (w *ExportWorker) processOne(ctx context.Context) error {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return err
	}
	data, err := w.buildZIP(ctx, job.MailboxID)
	if err != nil {
		return w.fail(ctx, job, err)
	}
	key := "exports/" + job.ID
	if err := w.store.PutExport(ctx, key, data); err != nil {
		return w.fail(ctx, job, err)
	}
	expiresAt := time.Now().Add(15 * 24 * time.Hour)
	_, err = w.client.ExportJob.UpdateOneID(job.ID).
		SetStatus("completed").
		SetObjectKey(key).
		SetSizeBytes(int64(len(data))).
		SetExpiresAt(expiresAt).
		ClearLockedBy().
		ClearLockedUntil().
		Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxExportCompleted, job.MailboxID, map[string]any{"event": events.MailboxExportCompleted, "export_id": job.ID, "mailbox_id": job.MailboxID, "object_key": key, "expires_at": expiresAt.Format(time.RFC3339)})
}

func (w *ExportWorker) claim(ctx context.Context) (exportJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return exportJob{}, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
UPDATE export_jobs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM export_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id
`, w.workerID, time.Now().Add(10*time.Minute))

	var job exportJob
	if err := row.Scan(&job.ID, &job.MailboxID); err != nil {
		if err == sql.ErrNoRows {
			return exportJob{}, false, nil
		}
		return exportJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return exportJob{}, false, err
	}
	return job, true, nil
}

func (w *ExportWorker) buildZIP(ctx context.Context, mailboxID string) ([]byte, error) {
	items, err := w.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mailboxID), mailboxmessage.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	for _, item := range items {
		msg, err := w.client.Message.Query().Where(message.IDEQ(item.MessageID)).Only(ctx)
		if err != nil {
			return nil, err
		}
		raw, err := w.store.GetMessage(ctx, msg.BlobKey)
		if err != nil {
			return nil, err
		}
		file, err := archive.Create(fmt.Sprintf("messages/%s.eml", msg.ID))
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(raw); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (w *ExportWorker) fail(ctx context.Context, job exportJob, cause error) error {
	_, err := w.client.ExportJob.UpdateOneID(job.ID).SetStatus("failed").SetLastError(map[string]any{"error": cause.Error()}).ClearLockedBy().ClearLockedUntil().Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxExportFailed, job.MailboxID, map[string]any{"event": events.MailboxExportFailed, "export_id": job.ID, "mailbox_id": job.MailboxID, "error": cause.Error()})
}

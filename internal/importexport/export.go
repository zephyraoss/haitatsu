package importexport

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/exportjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/ids"
)

type Store interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
	GetObjectReader(ctx context.Context, key string) (io.ReadCloser, error)
	PutMessage(ctx context.Context, key string, data []byte) error
	PutExportStream(ctx context.Context, key string, data io.Reader, size int64) error
}

type ExportWorker struct {
	db       *sql.DB
	client   *ent.Client
	store    Store
	events   *events.Service
	workerID string
	backend  database.Backend
}

type exportJob struct {
	ID        string
	MailboxID string
}

func NewExportWorker(db *sql.DB, client *ent.Client, store Store, events *events.Service, workerID string, backends ...database.Backend) *ExportWorker {
	backend := database.BackendPostgres
	if len(backends) > 0 {
		backend = backends[0]
	}
	return &ExportWorker{db: db, client: client, store: store, events: events, workerID: workerID, backend: backend}
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
	job, owner, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.renewLease(jobCtx, cancel, job.ID, owner)
	archive, size, err := w.buildZIP(jobCtx, job.MailboxID)
	if err != nil {
		if jobCtx.Err() != nil {
			w.release(job.ID, owner)
			return err
		}
		return w.fail(ctx, job, owner, err)
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	key := "exports/" + job.ID
	if err := w.store.PutExportStream(jobCtx, key, archive, size); err != nil {
		if jobCtx.Err() != nil {
			w.release(job.ID, owner)
			return err
		}
		return w.fail(ctx, job, owner, err)
	}
	expiresAt := time.Now().Add(15 * 24 * time.Hour)
	n, err := w.client.ExportJob.Update().
		Where(exportjob.IDEQ(job.ID), exportjob.LockedByEQ(owner)).
		SetStatus("completed").
		SetObjectKey(key).
		SetSizeBytes(size).
		SetExpiresAt(expiresAt).
		ClearLockedBy().
		ClearLockedUntil().
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	return w.events.Emit(ctx, events.MailboxExportCompleted, job.MailboxID, map[string]any{"event": events.MailboxExportCompleted, "export_id": job.ID, "mailbox_id": job.MailboxID, "object_key": key, "expires_at": expiresAt.Format(time.RFC3339)})
}

func (w *ExportWorker) claim(ctx context.Context) (exportJob, string, bool, error) {
	owner := w.workerID + "/" + ids.New().String()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return exportJob{}, "", false, err
	}
	defer tx.Rollback()

	query := `
UPDATE export_jobs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM export_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id
`
	now := time.Now().UTC()
	args := []any{owner, now.Add(jobLeaseDuration)}
	if w.backend.SQLiteFamily() {
		query = `
UPDATE export_jobs SET locked_by = ?, locked_until = ?, status = 'processing', updated_at = ?
WHERE id = (
  SELECT id FROM export_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
  ORDER BY created_at
  LIMIT 1
)
AND status = 'queued' AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
RETURNING id, mailbox_id
`
		args = []any{owner, now.Add(jobLeaseDuration), now, now, now}
	}
	row := tx.QueryRowContext(ctx, query, args...)

	var job exportJob
	if err := row.Scan(&job.ID, &job.MailboxID); err != nil {
		if err == sql.ErrNoRows {
			return exportJob{}, "", false, nil
		}
		return exportJob{}, "", false, err
	}
	if err := tx.Commit(); err != nil {
		return exportJob{}, "", false, err
	}
	return job, owner, true, nil
}

func (w *ExportWorker) renewLease(ctx context.Context, cancel context.CancelFunc, jobID string, owner string) {
	ticker := time.NewTicker(jobLeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := w.client.ExportJob.Update().
				Where(exportjob.IDEQ(jobID), exportjob.StatusEQ("processing"), exportjob.LockedByEQ(owner)).
				SetLockedUntil(time.Now().Add(jobLeaseDuration)).
				Save(ctx)
			if err != nil {
				continue
			}
			if n == 0 {
				cancel()
				return
			}
		}
	}
}

func (w *ExportWorker) release(jobID string, owner string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = w.client.ExportJob.Update().
		Where(exportjob.IDEQ(jobID), exportjob.LockedByEQ(owner)).
		SetStatus("queued").ClearLockedBy().ClearLockedUntil().
		Save(ctx)
}

func (w *ExportWorker) buildZIP(ctx context.Context, mailboxID string) (*os.File, int64, error) {
	items, err := w.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mailboxID), mailboxmessage.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	tmp, err := os.CreateTemp("", "haitatsu-export-*.zip")
	if err != nil {
		return nil, 0, err
	}
	discard := func(err error) (*os.File, int64, error) {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	messageIDs := make([]string, 0, len(items))
	for _, item := range items {
		messageIDs = append(messageIDs, item.MessageID)
	}
	blobKeys := make(map[string]string, len(messageIDs))
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		batch, err := w.client.Message.Query().
			Where(message.IDIn(messageIDs[start:end]...)).
			Select(message.FieldID, message.FieldBlobKey).
			All(ctx)
		if err != nil {
			return discard(err)
		}
		for _, msg := range batch {
			blobKeys[msg.ID] = msg.BlobKey
		}
	}
	archive := zip.NewWriter(tmp)
	for _, messageID := range messageIDs {
		blobKey, ok := blobKeys[messageID]
		if !ok {
			return discard(fmt.Errorf("message %s not found", messageID))
		}
		if err := w.appendMessage(ctx, archive, messageID, blobKey); err != nil {
			return discard(err)
		}
	}
	if err := archive.Close(); err != nil {
		return discard(err)
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return discard(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return discard(err)
	}
	return tmp, size, nil
}

func (w *ExportWorker) appendMessage(ctx context.Context, archive *zip.Writer, messageID string, blobKey string) error {
	reader, err := w.store.GetObjectReader(ctx, blobKey)
	if err != nil {
		return err
	}
	defer reader.Close()
	file, err := archive.Create(fmt.Sprintf("messages/%s.eml", messageID))
	if err != nil {
		return err
	}
	_, err = io.Copy(file, reader)
	return err
}

func (w *ExportWorker) fail(ctx context.Context, job exportJob, owner string, cause error) error {
	n, err := w.client.ExportJob.Update().
		Where(exportjob.IDEQ(job.ID), exportjob.LockedByEQ(owner)).
		SetStatus("failed").SetLastError(map[string]any{"error": cause.Error()}).ClearLockedBy().ClearLockedUntil().
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	return w.events.Emit(ctx, events.MailboxExportFailed, job.MailboxID, map[string]any{"event": events.MailboxExportFailed, "export_id": job.ID, "mailbox_id": job.MailboxID, "error": cause.Error()})
}

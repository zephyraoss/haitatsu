package importexport

import (
	"context"
	"database/sql"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/events"
)

type ImportWorker struct {
	db       *sql.DB
	client   *ent.Client
	events   *events.Service
	workerID string
}

type importJob struct {
	ID        string
	MailboxID string
	Source    string
}

func NewImportWorker(db *sql.DB, client *ent.Client, events *events.Service, workerID string) *ImportWorker {
	return &ImportWorker{db: db, client: client, events: events, workerID: workerID}
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
	message := "import source is not implemented yet"
	_, err = w.client.ImportJob.UpdateOneID(job.ID).SetStatus("failed").SetLastError(map[string]any{"error": message}).ClearLockedBy().ClearLockedUntil().Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxImportFailed, job.MailboxID, map[string]any{"event": events.MailboxImportFailed, "import_id": job.ID, "mailbox_id": job.MailboxID, "error": message})
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
RETURNING id, mailbox_id, source_type
`, w.workerID, time.Now().Add(10*time.Minute))

	var job importJob
	if err := row.Scan(&job.ID, &job.MailboxID, &job.Source); err != nil {
		if err == sql.ErrNoRows {
			return importJob{}, false, nil
		}
		return importJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return importJob{}, false, err
	}
	return job, true, nil
}

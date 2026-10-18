package cleanup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/authlockout"
	"github.com/zephyraoss/haitatsu/internal/database/ent/eventlog"
	"github.com/zephyraoss/haitatsu/internal/database/ent/exportjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/importjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/database/ent/outboundjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

type Store interface {
	DeleteObject(ctx context.Context, key string) error
}

type Worker struct {
	client  *ent.Client
	store   Store
	mail    *mailstore.Store
	metrics *metrics.Metrics
}

func New(client *ent.Client, store Store, mail *mailstore.Store, metrics *metrics.Metrics) *Worker {
	return &Worker{client: client, store: store, mail: mail, metrics: metrics}
}

func (w *Worker) Run(ctx context.Context) {
	go w.sampleQueueDepth(ctx)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		w.runOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.runOnce(ctx)
			}
		}
	}()
}

func (w *Worker) runOnce(ctx context.Context) {
	if err := w.Cleanup(ctx); err != nil {
		slog.Error("cleanup failed", "error", err)
	}
}

func (w *Worker) Cleanup(ctx context.Context) error {
	return errors.Join(
		w.cleanupTrash(ctx),
		w.cleanupDeletedMailboxes(ctx),
		w.cleanupExpiredExports(ctx),
		w.cleanupOrphanedMessages(ctx),
		w.recoverStaleLeases(ctx),
		w.cleanupAuthLockouts(ctx),
		w.updateQueueDepth(ctx),
	)
}

func ignoreNotFound(err error) error {
	if ent.IsNotFound(err) {
		return nil
	}
	return err
}

func (w *Worker) cleanupTrash(ctx context.Context) error {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	folders, err := w.client.Folder.Query().Where(folder.NameEQ("Trash")).All(ctx)
	if err != nil {
		return err
	}
	for _, item := range folders {
		expired, err := w.client.MailboxMessage.Query().
			Where(mailboxmessage.FolderIDEQ(item.ID), mailboxmessage.DeletedAtIsNil(), mailboxmessage.UpdatedAtLT(cutoff)).
			All(ctx)
		if err != nil {
			return err
		}
		if err := w.mail.SoftDeleteMany(ctx, expired); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) cleanupDeletedMailboxes(ctx context.Context) error {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	mailboxes, err := w.client.Mailbox.Query().Where(mailbox.StatusEQ("deleted"), mailbox.DeletedAtLTE(cutoff)).All(ctx)
	if err != nil {
		return err
	}
	for _, mbox := range mailboxes {
		if err := w.mail.PurgeMailbox(ctx, mbox.ID); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) cleanupExpiredExports(ctx context.Context) error {
	jobs, err := w.client.ExportJob.Query().Where(exportjob.ExpiresAtLTE(time.Now()), exportjob.ObjectKeyNotNil()).All(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := w.store.DeleteObject(ctx, job.ObjectKey); err != nil {
			return err
		}
		if err := ignoreNotFound(w.client.ExportJob.DeleteOneID(job.ID).Exec(ctx)); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) cleanupOrphanedMessages(ctx context.Context) error {
	cutoff := time.Now().Add(-24 * time.Hour)
	retention := time.Now().Add(-30 * 24 * time.Hour)
	var lastID string
	for {
		query := w.client.Message.Query().Where(message.CreatedAtLT(cutoff), unreferencedMessage(retention)).Order(message.ByID()).Limit(200)
		if lastID != "" {
			query.Where(message.IDGT(lastID))
		}
		messages, err := query.All(ctx)
		if err != nil {
			return err
		}
		for _, msg := range messages {
			lastID = msg.ID
			if err := w.deleteMessageRows(ctx, msg, retention); err != nil {
				return err
			}
		}
		if len(messages) < 200 {
			return nil
		}
	}
}

func unreferencedMessage(retention time.Time) predicate.Message {
	return func(s *sql.Selector) {
		builder := sql.Dialect(s.Dialect())
		items := builder.Table(mailboxmessage.Table)
		jobs := builder.Table(outboundjob.Table)
		s.Where(sql.And(
			sql.NotExists(builder.Select(items.C(mailboxmessage.FieldID)).From(items).Where(sql.And(
				sql.ColumnsEQ(items.C(mailboxmessage.FieldMessageID), s.C(message.FieldID)),
				sql.Or(sql.IsNull(items.C(mailboxmessage.FieldDeletedAt)), sql.GTE(items.C(mailboxmessage.FieldDeletedAt), retention)),
			))),
			sql.NotExists(builder.Select(jobs.C(outboundjob.FieldID)).From(jobs).Where(sql.And(
				sql.ColumnsEQ(jobs.C(outboundjob.FieldMessageID), s.C(message.FieldID)),
				sql.In(jobs.C(outboundjob.FieldStatus), "queued", "retry", "processing"),
			))),
		))
	}
}

func (w *Worker) deleteMessageRows(ctx context.Context, msg *ent.Message, retention time.Time) error {
	tx, err := w.client.Tx(ctx)
	if err != nil {
		return err
	}
	deleted, err := deleteMessageInTx(ctx, tx.Client(), msg.ID, retention)
	if err != nil || deleted == 0 {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if msg.BlobKey == "" {
		return nil
	}
	return w.store.DeleteObject(ctx, msg.BlobKey)
}

func deleteMessageInTx(ctx context.Context, client *ent.Client, messageID string, retention time.Time) (int, error) {
	ids, err := client.MailboxMessage.Query().Where(mailboxmessage.MessageIDEQ(messageID), mailboxmessage.DeletedAtLT(retention)).IDs(ctx)
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if err := mailstore.DeleteLabelLinks(ctx, client, ids); err != nil {
			return 0, err
		}
		if _, err := client.MailboxMessage.Delete().Where(mailboxmessage.IDIn(ids...), mailboxmessage.DeletedAtLT(retention)).Exec(ctx); err != nil {
			return 0, err
		}
	}
	return client.Message.Delete().Where(message.IDEQ(messageID), unreferencedMessage(retention)).Exec(ctx)
}

func (w *Worker) recoverStaleLeases(ctx context.Context) error {
	now := time.Now()
	if _, err := w.client.OutboundJob.Update().Where(outboundjob.StatusEQ("processing"), outboundjob.LockedUntilLTE(now)).SetStatus("retry").ClearLockedBy().ClearLockedUntil().Save(ctx); err != nil {
		return err
	}
	if _, err := w.client.EventLog.Update().Where(eventlog.StatusEQ("processing"), eventlog.LockedUntilLTE(now)).SetStatus("retry").ClearLockedBy().ClearLockedUntil().Save(ctx); err != nil {
		return err
	}
	if _, err := w.client.ExportJob.Update().Where(exportjob.StatusEQ("processing"), exportjob.LockedUntilLTE(now)).SetStatus("queued").ClearLockedBy().ClearLockedUntil().Save(ctx); err != nil {
		return err
	}
	_, err := w.client.ImportJob.Update().Where(importjob.StatusEQ("processing"), importjob.LockedUntilLTE(now)).SetStatus("queued").ClearLockedBy().ClearLockedUntil().Save(ctx)
	return err
}

func (w *Worker) cleanupAuthLockouts(ctx context.Context) error {
	_, err := w.client.AuthLockout.Delete().Where(authlockout.WindowStartLT(time.Now().Add(-24 * time.Hour))).Exec(ctx)
	return err
}

func (w *Worker) sampleQueueDepth(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	_ = w.updateQueueDepth(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.updateQueueDepth(ctx)
		}
	}
}

func (w *Worker) updateQueueDepth(ctx context.Context) error {
	outbound, err := w.client.OutboundJob.Query().Where(outboundjob.StatusIn("queued", "retry")).Count(ctx)
	if err != nil {
		return err
	}
	events, err := w.client.EventLog.Query().Where(eventlog.StatusIn("queued", "retry")).Count(ctx)
	if err != nil {
		return err
	}
	exports, err := w.client.ExportJob.Query().Where(exportjob.StatusEQ("queued")).Count(ctx)
	if err != nil {
		return err
	}
	imports, err := w.client.ImportJob.Query().Where(importjob.StatusEQ("queued")).Count(ctx)
	if err != nil {
		return err
	}
	w.metrics.SetQueueDepth(outbound + events + exports + imports)
	return nil
}

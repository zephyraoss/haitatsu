package cleanup

import (
	"context"
	"log/slog"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/authlockout"
	"github.com/zephyraoss/haitatsu/internal/database/ent/eventlog"
	"github.com/zephyraoss/haitatsu/internal/database/ent/exportjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/importjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/database/ent/outboundjob"
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
	if err := w.cleanupTrash(ctx); err != nil {
		return err
	}
	if err := w.cleanupDeletedMailboxes(ctx); err != nil {
		return err
	}
	if err := w.cleanupExpiredExports(ctx); err != nil {
		return err
	}
	if err := w.cleanupOrphanedMessages(ctx); err != nil {
		return err
	}
	if err := w.recoverStaleLeases(ctx); err != nil {
		return err
	}
	if err := w.cleanupAuthLockouts(ctx); err != nil {
		return err
	}
	return w.updateQueueDepth(ctx)
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
		if err := w.client.ExportJob.DeleteOneID(job.ID).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) cleanupOrphanedMessages(ctx context.Context) error {
	cutoff := time.Now().Add(-24 * time.Hour)
	var lastID string
	for {
		query := w.client.Message.Query().Where(message.CreatedAtLT(cutoff)).Order(message.ByID()).Limit(200)
		if lastID != "" {
			query.Where(message.IDGT(lastID))
		}
		messages, err := query.All(ctx)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return nil
		}
		for _, msg := range messages {
			lastID = msg.ID
			if err := w.maybeDeleteMessage(ctx, msg); err != nil {
				return err
			}
		}
		if len(messages) < 200 {
			return nil
		}
	}
}

func (w *Worker) maybeDeleteMessage(ctx context.Context, msg *ent.Message) error {
	active, err := w.client.MailboxMessage.Query().Where(mailboxmessage.MessageIDEQ(msg.ID), mailboxmessage.DeletedAtIsNil()).Count(ctx)
	if err != nil {
		return err
	}
	if active > 0 {
		return nil
	}
	pending, err := w.client.OutboundJob.Query().Where(outboundjob.MessageIDEQ(msg.ID), outboundjob.StatusIn("queued", "retry", "processing")).Count(ctx)
	if err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	return w.deleteMessageRows(ctx, msg)
}

func (w *Worker) deleteMessageRows(ctx context.Context, msg *ent.Message) error {
	items, err := w.client.MailboxMessage.Query().Where(mailboxmessage.MessageIDEQ(msg.ID)).All(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for _, item := range items {
		if item.DeletedAt == nil || item.DeletedAt.After(cutoff) {
			return nil
		}
	}
	for _, item := range items {
		if _, err := w.client.MailboxMessageLabel.Delete().Where(mailboxmessagelabel.MailboxMessageIDEQ(item.ID)).Exec(ctx); err != nil {
			return err
		}
		if err := w.client.MailboxMessage.DeleteOneID(item.ID).Exec(ctx); err != nil {
			return err
		}
	}
	if msg.BlobKey != "" {
		if err := w.store.DeleteObject(ctx, msg.BlobKey); err != nil {
			return err
		}
	}
	return w.client.Message.DeleteOneID(msg.ID).Exec(ctx)
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

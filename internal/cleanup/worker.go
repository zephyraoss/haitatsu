package cleanup

import (
	"context"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
)

type Worker struct {
	client *ent.Client
}

func New(client *ent.Client) *Worker {
	return &Worker{client: client}
}

func (w *Worker) Run(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = w.Cleanup(ctx)
			}
		}
	}()
}

func (w *Worker) Cleanup(ctx context.Context) error {
	if err := w.cleanupTrash(ctx); err != nil {
		return err
	}
	return w.cleanupDeletedMailboxes(ctx)
}

func (w *Worker) cleanupTrash(ctx context.Context) error {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	folders, err := w.client.Folder.Query().Where(folder.NameEQ("Trash")).All(ctx)
	if err != nil {
		return err
	}
	for _, item := range folders {
		_, err := w.client.MailboxMessage.Update().
			Where(mailboxmessage.FolderIDEQ(item.ID), mailboxmessage.DeletedAtIsNil(), mailboxmessage.UpdatedAtLT(cutoff)).
			SetDeletedAt(time.Now()).
			Save(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) cleanupDeletedMailboxes(ctx context.Context) error {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	_, err := w.client.Mailbox.Delete().Where(mailbox.StatusEQ("deleted"), mailbox.DeletedAtLTE(cutoff)).Exec(ctx)
	return err
}

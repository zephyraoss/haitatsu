package mailstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
)

var ErrOverQuota = errors.New("mailbox over quota")

var SystemFolders = []string{"INBOX", "Sent", "Drafts", "Trash", "Junk", "Archive"}

type Store struct {
	client   *ent.Client
	notifier *Notifier
}

type Attach struct {
	MailboxID       string
	MessageID       string
	FolderID        string
	SizeBytes       int64
	OriginalRcpt    string
	BaseRcpt        string
	PlusTag         string
	ResolvedRouteID string
	Flags           Flags
	CreatedAt       time.Time
	EnforceQuota    bool
}

func New(client *ent.Client, notifier *Notifier) *Store {
	return &Store{client: client, notifier: notifier}
}

func (s *Store) Client() *ent.Client { return s.client }

func (s *Store) Notifier() *Notifier { return s.notifier }

func (s *Store) CreateDefaultFolders(ctx context.Context, mailboxID string) error {
	for _, name := range SystemFolders {
		if _, err := s.client.Folder.Create().SetMailboxID(mailboxID).SetName(name).SetSystem(true).SetUIDValidity(NewUIDValidity()).Save(ctx); err != nil && !ent.IsConstraintError(err) {
			return err
		}
	}
	return nil
}

func (s *Store) CreateFolder(ctx context.Context, mailboxID string, name string, system bool) (*ent.Folder, error) {
	return s.client.Folder.Create().SetMailboxID(mailboxID).SetName(name).SetSystem(system).SetUIDValidity(NewUIDValidity()).Save(ctx)
}

func (s *Store) CreateLabel(ctx context.Context, mailboxID string, name string) (*ent.Label, error) {
	return s.client.Label.Create().SetMailboxID(mailboxID).SetName(name).SetUIDValidity(NewUIDValidity()).Save(ctx)
}

func NewUIDValidity() uint32 {
	return uint32(time.Now().Unix())
}

func (s *Store) FolderByName(ctx context.Context, mailboxID string, name string) (*ent.Folder, error) {
	return s.client.Folder.Query().Where(folder.MailboxIDEQ(mailboxID), folder.NameEQ(name)).Only(ctx)
}

func (s *Store) Attach(ctx context.Context, params Attach) (*ent.MailboxMessage, error) {
	if params.EnforceQuota {
		mbox, err := s.client.Mailbox.Get(ctx, params.MailboxID)
		if err != nil {
			return nil, err
		}
		if OverQuotaWith(mbox, params.SizeBytes) {
			return nil, ErrOverQuota
		}
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	item, err := attachInTx(ctx, tx.Client(), params)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.notifier.Publish(ctx, Change{MailboxID: params.MailboxID, Container: FolderContainer(params.FolderID), Kind: ChangeExists, UID: item.UID})
	return item, nil
}

func attachInTx(ctx context.Context, client *ent.Client, params Attach) (*ent.MailboxMessage, error) {
	uid, err := allocateFolderUID(ctx, client, params.FolderID)
	if err != nil {
		return nil, err
	}
	create := client.MailboxMessage.Create().
		SetMailboxID(params.MailboxID).
		SetMessageID(params.MessageID).
		SetFolderID(params.FolderID).
		SetUID(uid).
		SetOriginalRcpt(params.OriginalRcpt).
		SetBaseRcpt(params.BaseRcpt)
	if params.PlusTag != "" {
		create.SetPlusTag(params.PlusTag)
	}
	if params.ResolvedRouteID != "" {
		create.SetResolvedRouteID(params.ResolvedRouteID)
	}
	if !params.CreatedAt.IsZero() {
		create.SetCreatedAt(params.CreatedAt.UTC())
	}
	keywords := params.Flags.Keywords
	if keywords == nil {
		keywords = []string{}
	}
	item, err := create.SetRead(params.Flags.Seen).SetAnswered(params.Flags.Answered).SetFlagged(params.Flags.Flagged).SetImapDeleted(params.Flags.Deleted).SetDraft(params.Flags.Draft).SetKeywords(keywords).Save(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Mailbox.UpdateOneID(params.MailboxID).AddUsedBytes(params.SizeBytes).Save(ctx); err != nil {
		return nil, err
	}
	return item, nil
}

func allocateFolderUID(ctx context.Context, client *ent.Client, folderID string) (uint32, error) {
	updated, err := client.Folder.UpdateOneID(folderID).AddUIDNext(1).Save(ctx)
	if err != nil {
		return 0, err
	}
	return updated.UIDNext - 1, nil
}

func allocateLabelUID(ctx context.Context, client *ent.Client, labelID string) (uint32, error) {
	updated, err := client.Label.UpdateOneID(labelID).AddUIDNext(1).Save(ctx)
	if err != nil {
		return 0, err
	}
	return updated.UIDNext - 1, nil
}

func (s *Store) Move(ctx context.Context, item *ent.MailboxMessage, folderID string) (*ent.MailboxMessage, error) {
	if item.FolderID == folderID && item.DeletedAt == nil {
		return item, nil
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	uid, err := allocateFolderUID(ctx, tx.Client(), folderID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	restored := item.DeletedAt != nil
	update := tx.Client().MailboxMessage.UpdateOneID(item.ID).SetFolderID(folderID).SetUID(uid).SetImapDeleted(false).ClearDeletedAt()
	moved, err := update.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if restored {
		size, err := messageSize(ctx, tx.Client(), item.MessageID)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if _, err := tx.Client().Mailbox.UpdateOneID(item.MailboxID).AddUsedBytes(size).Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if !restored {
		s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: FolderContainer(item.FolderID), Kind: ChangeExpunge, UID: item.UID})
	}
	s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: FolderContainer(folderID), Kind: ChangeExists, UID: uid})
	return moved, nil
}

func (s *Store) MoveToFolderName(ctx context.Context, item *ent.MailboxMessage, name string) (*ent.MailboxMessage, error) {
	target, err := s.FolderByName(ctx, item.MailboxID, name)
	if err != nil {
		return nil, err
	}
	return s.Move(ctx, item, target.ID)
}

func (s *Store) SoftDelete(ctx context.Context, item *ent.MailboxMessage) error {
	if item.DeletedAt != nil {
		return nil
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	if err := softDeleteInTx(ctx, tx.Client(), item); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: FolderContainer(item.FolderID), Kind: ChangeExpunge, UID: item.UID})
	return nil
}

func softDeleteInTx(ctx context.Context, client *ent.Client, item *ent.MailboxMessage) error {
	n, err := client.MailboxMessage.Update().Where(mailboxmessage.IDEQ(item.ID), mailboxmessage.DeletedAtIsNil()).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil || n == 0 {
		return err
	}
	size, err := messageSize(ctx, client, item.MessageID)
	if err != nil {
		return err
	}
	_, err = client.Mailbox.UpdateOneID(item.MailboxID).AddUsedBytes(-size).Save(ctx)
	return err
}

func (s *Store) SoftDeleteMany(ctx context.Context, items []*ent.MailboxMessage) error {
	for _, item := range items {
		if err := s.SoftDelete(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetFlags(ctx context.Context, item *ent.MailboxMessage, flags Flags) (*ent.MailboxMessage, error) {
	updated, err := applyFlags(s.client.MailboxMessage.UpdateOneID(item.ID), flags).Save(ctx)
	if err != nil {
		return nil, err
	}
	s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: FolderContainer(item.FolderID), Kind: ChangeFlags, UID: updated.UID, Flags: flags.List()})
	s.publishLabelFlagChanges(ctx, updated, flags)
	return updated, nil
}

func (s *Store) publishLabelFlagChanges(ctx context.Context, item *ent.MailboxMessage, flags Flags) {
	links, err := s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.MailboxMessageIDEQ(item.ID)).All(ctx)
	if err != nil {
		return
	}
	for _, link := range links {
		s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: LabelContainer(link.LabelID), Kind: ChangeFlags, UID: link.UID, Flags: flags.List()})
	}
}

func (s *Store) AddLabel(ctx context.Context, item *ent.MailboxMessage, labelID string) (*ent.MailboxMessageLabel, error) {
	existing, err := s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.MailboxMessageIDEQ(item.ID), mailboxmessagelabel.LabelIDEQ(labelID)).First(ctx)
	if err == nil {
		return existing, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	uid, err := allocateLabelUID(ctx, tx.Client(), labelID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	link, err := tx.Client().MailboxMessageLabel.Create().SetMailboxMessageID(item.ID).SetLabelID(labelID).SetUID(uid).Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			return s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.MailboxMessageIDEQ(item.ID), mailboxmessagelabel.LabelIDEQ(labelID)).Only(ctx)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: LabelContainer(labelID), Kind: ChangeExists, UID: uid})
	return link, nil
}

func (s *Store) RemoveLabel(ctx context.Context, item *ent.MailboxMessage, labelID string) error {
	link, err := s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.MailboxMessageIDEQ(item.ID), mailboxmessagelabel.LabelIDEQ(labelID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := s.client.MailboxMessageLabel.DeleteOneID(link.ID).Exec(ctx); err != nil && !ent.IsNotFound(err) {
		return err
	}
	s.notifier.Publish(ctx, Change{MailboxID: item.MailboxID, Container: LabelContainer(labelID), Kind: ChangeExpunge, UID: link.UID})
	return nil
}

func (s *Store) LabelByName(ctx context.Context, mailboxID string, name string) (*ent.Label, error) {
	return s.client.Label.Query().Where(label.MailboxIDEQ(mailboxID), label.NameEQ(name)).Only(ctx)
}

func (s *Store) RecomputeUsedBytes(ctx context.Context, mailboxID string) (int64, error) {
	items, err := s.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mailboxID), mailboxmessage.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.MessageID)
	}
	var total int64
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		messages, err := s.client.Message.Query().Where(message.IDIn(ids[start:end]...)).All(ctx)
		if err != nil {
			return 0, err
		}
		for _, msg := range messages {
			total += msg.SizeBytes
		}
	}
	if _, err := s.client.Mailbox.UpdateOneID(mailboxID).SetUsedBytes(total).Save(ctx); err != nil {
		return 0, err
	}
	return total, nil
}

func messageSize(ctx context.Context, client *ent.Client, messageID string) (int64, error) {
	msg, err := client.Message.Query().Where(message.IDEQ(messageID)).Select(message.FieldSizeBytes).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	return msg.SizeBytes, nil
}

func OverQuota(mbox *ent.Mailbox) bool {
	if mbox == nil || mbox.QuotaBytes <= 0 {
		return false
	}
	return mbox.UsedBytes >= mbox.QuotaBytes
}

func OverQuotaWith(mbox *ent.Mailbox, additional int64) bool {
	if mbox == nil || mbox.QuotaBytes <= 0 {
		return false
	}
	return mbox.UsedBytes+additional > mbox.QuotaBytes
}

func (s *Store) PurgeMailbox(ctx context.Context, mailboxID string) error {
	items, err := s.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(mailboxID)).Select(mailboxmessage.FieldID).All(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err := s.client.MailboxMessageLabel.Delete().Where(mailboxmessagelabel.MailboxMessageIDEQ(item.ID)).Exec(ctx); err != nil {
			return err
		}
	}
	if _, err := s.client.MailboxMessage.Delete().Where(mailboxmessage.MailboxIDEQ(mailboxID)).Exec(ctx); err != nil {
		return err
	}
	if _, err := s.client.Label.Delete().Where(label.MailboxIDEQ(mailboxID)).Exec(ctx); err != nil {
		return err
	}
	if _, err := s.client.Folder.Delete().Where(folder.MailboxIDEQ(mailboxID)).Exec(ctx); err != nil {
		return err
	}
	err = s.client.Mailbox.DeleteOneID(mailboxID).Exec(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Store) MailboxOverQuota(ctx context.Context, mailboxID string, additional int64) (bool, error) {
	mbox, err := s.client.Mailbox.Query().Where(mailbox.IDEQ(mailboxID)).Only(ctx)
	if err != nil {
		return false, err
	}
	return OverQuotaWith(mbox, additional), nil
}

func (s *Store) ActiveMessagesInFolder(ctx context.Context, folderID string) ([]*ent.MailboxMessage, error) {
	return s.client.MailboxMessage.Query().Where(mailboxmessage.FolderIDEQ(folderID), mailboxmessage.DeletedAtIsNil()).Order(mailboxmessage.ByUID()).All(ctx)
}

func (s *Store) BackfillUIDs(ctx context.Context) error {
	folders, err := s.client.Folder.Query().All(ctx)
	if err != nil {
		return err
	}
	for _, item := range folders {
		if err := s.backfillFolder(ctx, item); err != nil {
			return fmt.Errorf("backfill folder %s: %w", item.ID, err)
		}
	}
	labels, err := s.client.Label.Query().All(ctx)
	if err != nil {
		return err
	}
	for _, item := range labels {
		if err := s.backfillLabel(ctx, item); err != nil {
			return fmt.Errorf("backfill label %s: %w", item.ID, err)
		}
	}
	return nil
}

func (s *Store) backfillFolder(ctx context.Context, item *ent.Folder) error {
	messages, err := s.client.MailboxMessage.Query().Where(mailboxmessage.FolderIDEQ(item.ID), mailboxmessage.UIDEQ(0)).Order(mailboxmessage.ByCreatedAt(), mailboxmessage.ByID()).All(ctx)
	if err != nil {
		return err
	}
	next := item.UIDNext
	if next == 0 {
		next = 1
	}
	for _, msg := range messages {
		if _, err := s.client.MailboxMessage.UpdateOneID(msg.ID).SetUID(next).Save(ctx); err != nil {
			return err
		}
		next++
	}
	update := s.client.Folder.UpdateOneID(item.ID).SetUIDNext(next)
	if item.UIDValidity == 0 {
		update.SetUIDValidity(NewUIDValidity())
	}
	_, err = update.Save(ctx)
	return err
}

func (s *Store) backfillLabel(ctx context.Context, item *ent.Label) error {
	links, err := s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.LabelIDEQ(item.ID), mailboxmessagelabel.UIDEQ(0)).Order(mailboxmessagelabel.ByCreatedAt(), mailboxmessagelabel.ByID()).All(ctx)
	if err != nil {
		return err
	}
	next := item.UIDNext
	if next == 0 {
		next = 1
	}
	for _, link := range links {
		if _, err := s.client.MailboxMessageLabel.UpdateOneID(link.ID).SetUID(next).Save(ctx); err != nil {
			return err
		}
		next++
	}
	update := s.client.Label.UpdateOneID(item.ID).SetUIDNext(next)
	if item.UIDValidity == 0 {
		update.SetUIDValidity(NewUIDValidity())
	}
	_, err = update.Save(ctx)
	return err
}

func (s *Store) RecomputeAllUsedBytes(ctx context.Context) error {
	mailboxes, err := s.client.Mailbox.Query().Select(mailbox.FieldID).All(ctx)
	if err != nil {
		return err
	}
	for _, mbox := range mailboxes {
		if _, err := s.RecomputeUsedBytes(ctx, mbox.ID); err != nil {
			return err
		}
	}
	return nil
}

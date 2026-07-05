package imapserver

import (
	"context"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/ids"
)

func (s *session) Move(w *goimapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	if strings.HasPrefix(dest, labelPrefix) || strings.HasPrefix(s.selectedName, labelPrefix) {
		return unsupported("move involving labels")
	}
	if dest == s.selectedName {
		return nil
	}
	ctx := context.Background()
	target, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(dest)).Only(ctx)
	if err != nil {
		return err
	}
	items, err := s.messages(s.selectedName)
	if err != nil {
		return err
	}
	movedIDs := make([]string, 0)
	movedSeqs := make([]uint32, 0)
	var sourceUIDs imap.UIDSet
	for index, item := range items {
		seq := uint32(index + 1)
		if !containsNum(numSet, seq) {
			continue
		}
		if _, err := s.client.MailboxMessage.UpdateOneID(item.ID).SetFolderID(target.ID).Save(ctx); err != nil {
			return err
		}
		movedIDs = append(movedIDs, item.ID)
		movedSeqs = append(movedSeqs, seq)
		sourceUIDs.AddNum(imap.UID(seq))
	}
	if len(movedIDs) == 0 {
		return nil
	}
	destUIDs, err := s.uidsFor(dest, movedIDs)
	if err != nil {
		return err
	}
	if err := w.WriteCopyData(&imap.CopyData{UIDValidity: uidValidity(s.mailboxID), SourceUIDs: sourceUIDs, DestUIDs: destUIDs}); err != nil {
		return err
	}
	for i := len(movedSeqs) - 1; i >= 0; i-- {
		if err := w.WriteExpunge(movedSeqs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	items, err := s.messages(s.selectedName)
	if err != nil {
		return nil, err
	}
	selected := make([]*ent.MailboxMessage, 0)
	var sourceUIDs imap.UIDSet
	for index, item := range items {
		seq := uint32(index + 1)
		if containsNum(numSet, seq) {
			selected = append(selected, item)
			sourceUIDs.AddNum(imap.UID(seq))
		}
	}
	if len(selected) == 0 {
		return &imap.CopyData{UIDValidity: uidValidity(s.mailboxID)}, nil
	}
	if labelName, ok := strings.CutPrefix(dest, labelPrefix); ok {
		return s.copyToLabel(selected, labelName, sourceUIDs)
	}
	return s.copyToFolder(selected, dest, sourceUIDs)
}

func (s *session) copyToLabel(selected []*ent.MailboxMessage, labelName string, sourceUIDs imap.UIDSet) (*imap.CopyData, error) {
	ctx := context.Background()
	target, err := s.client.Label.Query().Where(label.MailboxIDEQ(s.mailboxID), label.NameEQ(labelName)).Only(ctx)
	if err != nil {
		return nil, err
	}
	linkedIDs := make([]string, 0, len(selected))
	for _, item := range selected {
		_, err := s.client.MailboxMessageLabel.Create().SetMailboxMessageID(item.ID).SetLabelID(target.ID).Save(ctx)
		if err != nil && !ent.IsConstraintError(err) {
			return nil, err
		}
		linkedIDs = append(linkedIDs, item.ID)
	}
	destUIDs, err := s.uidsFor(labelPrefix+labelName, linkedIDs)
	if err != nil {
		return nil, err
	}
	return &imap.CopyData{UIDValidity: uidValidity(s.mailboxID), SourceUIDs: sourceUIDs, DestUIDs: destUIDs}, nil
}

func (s *session) copyToFolder(selected []*ent.MailboxMessage, dest string, sourceUIDs imap.UIDSet) (*imap.CopyData, error) {
	ctx := context.Background()
	target, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(dest)).Only(ctx)
	if err != nil {
		return nil, err
	}
	copiedIDs := make([]string, 0, len(selected))
	for _, item := range selected {
		copied, err := s.duplicateInto(ctx, item, target.ID)
		if err != nil {
			return nil, err
		}
		copiedIDs = append(copiedIDs, copied.ID)
	}
	destUIDs, err := s.uidsFor(dest, copiedIDs)
	if err != nil {
		return nil, err
	}
	return &imap.CopyData{UIDValidity: uidValidity(s.mailboxID), SourceUIDs: sourceUIDs, DestUIDs: destUIDs}, nil
}

func (s *session) duplicateInto(ctx context.Context, item *ent.MailboxMessage, folderID string) (*ent.MailboxMessage, error) {
	source, err := s.client.Message.Get(ctx, item.MessageID)
	if err != nil {
		return nil, err
	}
	raw, err := s.store.GetMessage(ctx, source.BlobKey)
	if err != nil {
		return nil, err
	}
	messageID := ids.New().String()
	key := appendObjectKey(time.Now().UTC(), messageID)
	if err := s.store.PutMessage(ctx, key, raw); err != nil {
		return nil, err
	}
	create := s.client.Message.Create().
		SetID(messageID).
		SetTraceID(ids.New().String()).
		SetBlobKey(key).
		SetSha256(source.Sha256).
		SetSizeBytes(source.SizeBytes).
		SetHeaders(source.Headers).
		SetFromAddresses(source.FromAddresses).
		SetToAddresses(source.ToAddresses).
		SetCcAddresses(source.CcAddresses).
		SetBccAddresses(source.BccAddresses).
		SetSubject(source.Subject).
		SetNillableDate(source.Date).
		SetTextBodyExtract(source.TextBodyExtract).
		SetHTMLBodyExtract(source.HTMLBodyExtract).
		SetAttachments(source.Attachments).
		SetSpamScore(source.SpamScore).
		SetAuthResults(source.AuthResults)
	if source.RfcMessageID != "" {
		create.SetRfcMessageID(source.RfcMessageID)
	}
	msg, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	copied, err := s.client.MailboxMessage.Create().
		SetMailboxID(s.mailboxID).
		SetMessageID(msg.ID).
		SetFolderID(folderID).
		SetOriginalRcpt(item.OriginalRcpt).
		SetBaseRcpt(item.BaseRcpt).
		SetRead(item.Read).
		SetFlagged(item.Flagged).
		SetImapDeleted(item.ImapDeleted).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.client.Mailbox.UpdateOneID(s.mailboxID).AddUsedBytes(source.SizeBytes).Save(ctx); err != nil {
		return nil, err
	}
	return copied, nil
}

func (s *session) uidsFor(mailboxName string, mailboxMessageIDs []string) (imap.UIDSet, error) {
	items, err := s.messages(mailboxName)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(mailboxMessageIDs))
	for _, id := range mailboxMessageIDs {
		wanted[id] = true
	}
	var uids imap.UIDSet
	for index, item := range items {
		if wanted[item.ID] {
			uids.AddNum(imap.UID(index + 1))
		}
	}
	return uids, nil
}

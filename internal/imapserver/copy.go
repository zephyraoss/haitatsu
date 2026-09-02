package imapserver

import (
	"context"
	"errors"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

func (s *session) Move(w *goimapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	if s.view == nil {
		return errNotSelected
	}
	if s.readOnly {
		return errReadOnly
	}
	ctx := context.Background()
	target, err := s.resolve(ctx, dest)
	if err != nil {
		return err
	}
	if err := s.resync(ctx, expungeOnlyWriter{write: w.WriteExpunge}, syncMode{expunge: true, force: true}); err != nil {
		return err
	}
	if target.key == s.view.container.key {
		return nil
	}
	indexes := s.view.selected(numSet)
	if len(indexes) == 0 {
		return nil
	}
	var sourceUIDs, destUIDs imap.UIDSet
	removed := map[int]struct{}{}
	for _, index := range indexes {
		item := s.view.entries[index]
		mm, err := s.mailboxMessage(ctx, item)
		if err != nil {
			if ent.IsNotFound(err) {
				continue
			}
			return err
		}
		destUID, err := s.moveOne(ctx, mm, item, target)
		if err != nil {
			return err
		}
		sourceUIDs.AddNum(imap.UID(item.uid))
		destUIDs.AddNum(imap.UID(destUID))
		removed[index] = struct{}{}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := w.WriteCopyData(&imap.CopyData{UIDValidity: target.uidValidity, SourceUIDs: sourceUIDs, DestUIDs: destUIDs}); err != nil {
		return err
	}
	remaining := make([]entry, 0, len(s.view.entries))
	for index, item := range s.view.entries {
		if _, gone := removed[index]; gone {
			if err := w.WriteExpunge(uint32(len(remaining) + 1)); err != nil {
				return err
			}
			continue
		}
		remaining = append(remaining, item)
	}
	s.view.entries = remaining
	s.view.drainChanges()
	return nil
}

func (s *session) moveOne(ctx context.Context, mm *ent.MailboxMessage, item entry, target container) (uint32, error) {
	source := s.view.container
	switch {
	case source.isLabel() && target.isLabel():
		link, err := s.store.AddLabel(ctx, mm, target.label.ID)
		if err != nil {
			return 0, err
		}
		return link.UID, s.store.RemoveLabel(ctx, mm, source.label.ID)
	case source.isLabel():
		moved, err := s.store.Move(ctx, mm, target.folder.ID)
		if err != nil {
			return 0, err
		}
		return moved.UID, s.store.RemoveLabel(ctx, mm, source.label.ID)
	case target.isLabel():
		link, err := s.store.AddLabel(ctx, mm, target.label.ID)
		if err != nil {
			return 0, err
		}
		return link.UID, s.store.SoftDelete(ctx, mm)
	default:
		moved, err := s.store.Move(ctx, mm, target.folder.ID)
		if err != nil {
			return 0, err
		}
		return moved.UID, nil
	}
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	if s.view == nil {
		return nil, errNotSelected
	}
	ctx := context.Background()
	target, err := s.resolve(ctx, dest)
	if err != nil {
		return nil, err
	}
	if err := s.resync(ctx, nil, syncMode{}); err != nil {
		return nil, err
	}
	var sourceUIDs, destUIDs imap.UIDSet
	for _, index := range s.view.selected(numSet) {
		item := s.view.entries[index]
		if item.gone {
			continue
		}
		mm, err := s.mailboxMessage(ctx, item)
		if err != nil {
			if ent.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		destUID, err := s.copyOne(ctx, mm, target)
		if err != nil {
			return nil, err
		}
		sourceUIDs.AddNum(imap.UID(item.uid))
		destUIDs.AddNum(imap.UID(destUID))
	}
	return &imap.CopyData{UIDValidity: target.uidValidity, SourceUIDs: sourceUIDs, DestUIDs: destUIDs}, nil
}

func (s *session) copyOne(ctx context.Context, mm *ent.MailboxMessage, target container) (uint32, error) {
	if target.isLabel() {
		link, err := s.store.AddLabel(ctx, mm, target.label.ID)
		if err != nil {
			return 0, err
		}
		return link.UID, nil
	}
	if mm.FolderID == target.folder.ID {
		return mm.UID, nil
	}
	source, err := s.client.Message.Get(ctx, mm.MessageID)
	if err != nil {
		return 0, err
	}
	raw, err := s.blobs.GetMessage(ctx, source.BlobKey)
	if err != nil {
		return 0, err
	}
	msg, err := s.storeMessage(ctx, raw, messageMetadata(source))
	if err != nil {
		return 0, err
	}
	copied, err := s.store.Attach(ctx, mailstore.Attach{
		MailboxID:    s.mailboxID,
		MessageID:    msg.ID,
		FolderID:     target.folder.ID,
		SizeBytes:    source.SizeBytes,
		OriginalRcpt: mm.OriginalRcpt,
		BaseRcpt:     mm.BaseRcpt,
		Flags:        mailstore.FlagsOf(mm),
		CreatedAt:    mm.CreatedAt,
		EnforceQuota: true,
	})
	if err != nil {
		if errors.Is(err, mailstore.ErrOverQuota) {
			return 0, errOverQuota
		}
		return 0, err
	}
	return copied.UID, nil
}

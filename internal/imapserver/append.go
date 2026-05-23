package imapserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

func (s *session) appendMessage(mailboxName string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	if strings.HasPrefix(mailboxName, labelPrefix) {
		return nil, unsupported("append to labels")
	}
	ctx := context.Background()
	target, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(mailboxName)).Only(ctx)
	if err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	raw = mailparse.NormalizeMessage(raw)
	metadata := mailparse.Parse(raw)

	if metadata.RFCMessageID != "" {
		if uid, ok, err := s.appendUIDForExisting(ctx, target.ID, metadata.RFCMessageID); err != nil {
			return nil, err
		} else if ok {
			return &imap.AppendData{UID: uid, UIDValidity: uidValidity(s.mailboxID)}, nil
		}
	}

	messageID := ids.New().String()
	traceID := ids.New().String()
	key := appendObjectKey(time.Now().UTC(), messageID)
	if err := s.store.PutMessage(ctx, key, raw); err != nil {
		return nil, err
	}

	create := s.client.Message.Create().
		SetID(messageID).
		SetTraceID(traceID).
		SetBlobKey(key).
		SetSha256(sha256Hex(raw)).
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
	if _, err := create.Save(ctx); err != nil {
		return nil, err
	}

	mailboxMessageCreate := s.client.MailboxMessage.Create().
		SetMailboxID(s.mailboxID).
		SetMessageID(messageID).
		SetFolderID(target.ID).
		SetOriginalRcpt("").
		SetBaseRcpt("")
	if options != nil && !options.Time.IsZero() {
		mailboxMessageCreate.SetCreatedAt(options.Time.UTC())
	}
	read, flagged := false, false
	if options != nil {
		for _, flag := range options.Flags {
			switch flag {
			case imap.FlagSeen:
				read = true
			case imap.FlagFlagged:
				flagged = true
			}
		}
	}
	item, err := mailboxMessageCreate.SetRead(read).SetFlagged(flagged).Save(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.client.Mailbox.UpdateOneID(s.mailboxID).AddUsedBytes(int64(len(raw))).Save(ctx); err != nil {
		return nil, err
	}

	uid, err := s.uidForMailboxMessage(mailboxName, item.ID)
	if err != nil {
		return nil, err
	}
	return &imap.AppendData{UID: uid, UIDValidity: uidValidity(s.mailboxID)}, nil
}

func (s *session) appendUIDForExisting(ctx context.Context, folderID string, rfcMessageID string) (imap.UID, bool, error) {
	msg, err := s.client.Message.Query().Where(message.RfcMessageIDEQ(rfcMessageID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	item, err := s.client.MailboxMessage.Query().
		Where(
			mailboxmessage.MailboxIDEQ(s.mailboxID),
			mailboxmessage.MessageIDEQ(msg.ID),
			mailboxmessage.FolderIDEQ(folderID),
			mailboxmessage.DeletedAtIsNil(),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	folderEntity, err := s.client.Folder.Get(ctx, folderID)
	if err != nil {
		return 0, false, err
	}
	uid, err := s.uidForMailboxMessage(folderEntity.Name, item.ID)
	if err != nil {
		return 0, false, err
	}
	return uid, true, nil
}

func (s *session) uidForMailboxMessage(mailboxName string, mailboxMessageID string) (imap.UID, error) {
	items, err := s.messages(mailboxName)
	if err != nil {
		return 0, err
	}
	for index, item := range items {
		if item.ID == mailboxMessageID {
			return imap.UID(index + 1), nil
		}
	}
	return 0, fmt.Errorf("appended message not found in %q", mailboxName)
}

func appendObjectKey(t time.Time, messageID string) string {
	return fmt.Sprintf("messages/%04d/%02d/%02d/%s.eml", t.Year(), t.Month(), t.Day(), messageID)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

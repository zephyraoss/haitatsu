package imapserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

var errOverQuota = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeOverQuota, Text: "Mailbox is over quota"}
var errTooBig = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTooBig, Text: "Message exceeds append limit"}

func (s *session) Append(mailboxName string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	ctx := context.Background()
	c, err := s.resolve(ctx, mailboxName)
	if err != nil {
		return nil, err
	}
	if s.appendLimit > 0 && r.Size() > s.appendLimit {
		_, _ = io.Copy(io.Discard, r)
		return nil, errTooBig
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	raw = mailparse.NormalizeMessage(raw)
	flags := mailstore.Flags{}
	var createdAt time.Time
	if options != nil {
		flags = mailstore.ParseFlags(storeFlagStrings(options.Flags))
		createdAt = options.Time
	}
	if c.isLabel() {
		return s.appendToLabel(ctx, c, raw, flags, createdAt)
	}
	if strings.EqualFold(c.folder.Name, "Drafts") {
		flags.Draft = true
	}
	if over, err := s.store.MailboxOverQuota(ctx, s.mailboxID, int64(len(raw))); err != nil {
		return nil, err
	} else if over {
		return nil, errOverQuota
	}
	metadata := mailparse.Parse(raw)
	if existing, ok, err := s.existingAppend(ctx, c.folder.ID, metadata.RFCMessageID, sha256Hex(raw)); err != nil {
		return nil, err
	} else if ok {
		return &imap.AppendData{UID: imap.UID(existing.UID), UIDValidity: c.uidValidity}, nil
	}
	msg, err := s.storeMessage(ctx, raw, metadata)
	if err != nil {
		return nil, err
	}
	item, err := s.store.Attach(ctx, mailstore.Attach{
		MailboxID: s.mailboxID,
		MessageID: msg.ID,
		FolderID:  c.folder.ID,
		SizeBytes: int64(len(raw)),
		Flags:     flags,
		CreatedAt: createdAt,
	})
	if err != nil {
		if errors.Is(err, mailstore.ErrOverQuota) {
			return nil, errOverQuota
		}
		return nil, err
	}
	return &imap.AppendData{UID: imap.UID(item.UID), UIDValidity: c.uidValidity}, nil
}

func (s *session) appendToLabel(ctx context.Context, c container, raw []byte, flags mailstore.Flags, createdAt time.Time) (*imap.AppendData, error) {
	sha := sha256Hex(raw)
	metadata := mailparse.Parse(raw)
	existing, err := s.findMailboxMessage(ctx, metadata.RFCMessageID, sha)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		inbox, err := s.store.FolderByName(ctx, s.mailboxID, "INBOX")
		if err != nil {
			return nil, err
		}
		msg, err := s.storeMessage(ctx, raw, metadata)
		if err != nil {
			return nil, err
		}
		existing, err = s.store.Attach(ctx, mailstore.Attach{MailboxID: s.mailboxID, MessageID: msg.ID, FolderID: inbox.ID, SizeBytes: int64(len(raw)), Flags: flags, CreatedAt: createdAt, EnforceQuota: true})
		if err != nil {
			if errors.Is(err, mailstore.ErrOverQuota) {
				return nil, errOverQuota
			}
			return nil, err
		}
	}
	link, err := s.store.AddLabel(ctx, existing, c.label.ID)
	if err != nil {
		return nil, err
	}
	return &imap.AppendData{UID: imap.UID(link.UID), UIDValidity: c.uidValidity}, nil
}

func (s *session) findMailboxMessage(ctx context.Context, rfcMessageID string, sha string) (*ent.MailboxMessage, error) {
	query := s.client.Message.Query().Where(message.Sha256EQ(sha))
	if rfcMessageID != "" {
		query = s.client.Message.Query().Where(message.Or(message.Sha256EQ(sha), message.RfcMessageIDEQ(rfcMessageID)))
	}
	messageIDs, err := query.IDs(ctx)
	if err != nil || len(messageIDs) == 0 {
		return nil, err
	}
	item, err := s.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(s.mailboxID), mailboxmessage.MessageIDIn(messageIDs...), mailboxmessage.DeletedAtIsNil()).First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return item, err
}

func (s *session) existingAppend(ctx context.Context, folderID string, rfcMessageID string, sha string) (*ent.MailboxMessage, bool, error) {
	query := s.client.Message.Query().Where(message.Sha256EQ(sha))
	if rfcMessageID != "" {
		query = s.client.Message.Query().Where(message.Or(message.Sha256EQ(sha), message.RfcMessageIDEQ(rfcMessageID)))
	}
	messageIDs, err := query.IDs(ctx)
	if err != nil || len(messageIDs) == 0 {
		return nil, false, err
	}
	item, err := s.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(s.mailboxID), mailboxmessage.MessageIDIn(messageIDs...), mailboxmessage.FolderIDEQ(folderID), mailboxmessage.DeletedAtIsNil()).First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return item, true, nil
}

func (s *session) storeMessage(ctx context.Context, raw []byte, metadata mailparse.Metadata) (*ent.Message, error) {
	messageID := ids.New().String()
	key := messageObjectKey(time.Now().UTC(), messageID)
	if err := s.blobs.PutMessage(ctx, key, raw); err != nil {
		return nil, err
	}
	create := s.client.Message.Create().
		SetID(messageID).
		SetTraceID(ids.New().String()).
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
	return create.Save(ctx)
}

func messageObjectKey(t time.Time, messageID string) string {
	return fmt.Sprintf("messages/%04d/%02d/%02d/%s.eml", t.Year(), t.Month(), t.Day(), messageID)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

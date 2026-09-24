package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
)

func (s *session) Fetch(w *goimapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if s.view == nil {
		return errNotSelected
	}
	ctx := context.Background()
	if err := s.resync(ctx, nil, syncMode{}); err != nil {
		return err
	}
	needsBlob := options.BodyStructure != nil || len(options.BodySection) > 0 || len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0
	needsMessage := needsBlob || options.Envelope || options.RFC822Size
	needsItem := options.InternalDate
	selected := s.view.selected(numSet)
	var items map[string]*ent.MailboxMessage
	if needsItem {
		ids := make([]string, 0, len(selected))
		for _, index := range selected {
			if item := s.view.entries[index]; !item.gone {
				ids = append(ids, item.itemID)
			}
		}
		loaded, err := s.loadMailboxMessages(ctx, ids)
		if err != nil {
			return err
		}
		items = loaded
	}
	var msgs map[string]*ent.Message
	if needsMessage {
		ids := make([]string, 0, len(selected))
		for _, index := range selected {
			if item := s.view.entries[index]; !item.gone {
				ids = append(ids, item.messageID)
			}
		}
		loaded, err := s.loadMessages(ctx, ids)
		if err != nil {
			return err
		}
		msgs = loaded
	}
	for _, index := range selected {
		item := s.view.entries[index]
		if item.gone {
			continue
		}
		seq := uint32(index + 1)
		writer := w.CreateMessage(seq)
		writer.WriteUID(imap.UID(item.uid))
		if options.Flags {
			writer.WriteFlags(imapFlags(item.flags))
		}
		if needsItem {
			mm, ok := items[item.itemID]
			if !ok {
				_ = writer.Close()
				continue
			}
			writer.WriteInternalDate(mm.CreatedAt)
		}
		if needsMessage {
			msg, ok := msgs[item.messageID]
			if !ok {
				_ = writer.Close()
				continue
			}
			if options.RFC822Size {
				writer.WriteRFC822Size(msg.SizeBytes)
			}
			var raw []byte
			if needsBlob || (options.Envelope && len(msg.Headers) == 0) {
				loaded, err := s.blobs.GetMessage(ctx, msg.BlobKey)
				if err != nil {
					return err
				}
				raw = loaded
			}
			if options.Envelope {
				header := storedHeader(msg.Headers)
				if len(msg.Headers) == 0 {
					header = messageHeader(raw)
				}
				writer.WriteEnvelope(goimapserver.ExtractEnvelope(header))
			}
			if needsBlob {
				if err := writeBody(writer, raw, options); err != nil {
					return err
				}
				if !item.flags.Seen && !s.readOnly && marksSeen(options) {
					if err := s.markSeen(ctx, index, item); err != nil {
						return err
					}
					writer.WriteFlags(imapFlags(s.view.entries[index].flags))
				}
			}
		}
		if err := writer.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) loadMailboxMessages(ctx context.Context, ids []string) (map[string]*ent.MailboxMessage, error) {
	items := make(map[string]*ent.MailboxMessage, len(ids))
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		batch, err := s.client.MailboxMessage.Query().Where(mailboxmessage.IDIn(ids[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range batch {
			items[item.ID] = item
		}
	}
	return items, nil
}

func (s *session) loadMessages(ctx context.Context, ids []string) (map[string]*ent.Message, error) {
	msgs := make(map[string]*ent.Message, len(ids))
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		batch, err := s.client.Message.Query().Where(message.IDIn(ids[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, msg := range batch {
			msgs[msg.ID] = msg
		}
	}
	return msgs, nil
}

func storedHeader(values map[string][]string) textproto.Header {
	var header textproto.Header
	for key, list := range values {
		for _, value := range list {
			header.Add(key, value)
		}
	}
	return header
}

func (s *session) markSeen(ctx context.Context, index int, item entry) error {
	mm, err := s.client.MailboxMessage.Get(ctx, item.itemID)
	if err != nil {
		return err
	}
	flags := item.flags
	flags.Seen = true
	if _, err := s.store.SetFlags(ctx, mm, flags); err != nil {
		return err
	}
	s.view.entries[index].flags = flags
	return nil
}

func marksSeen(options *imap.FetchOptions) bool {
	for _, section := range options.BodySection {
		if !section.Peek {
			return true
		}
	}
	for _, section := range options.BinarySection {
		if !section.Peek {
			return true
		}
	}
	return false
}

func writeBody(writer *goimapserver.FetchResponseWriter, raw []byte, options *imap.FetchOptions) error {
	if options.BodyStructure != nil {
		writer.WriteBodyStructure(goimapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}
	for _, section := range options.BodySection {
		body := goimapserver.ExtractBodySection(bytes.NewReader(raw), section)
		part := writer.WriteBodySection(section, int64(len(body)))
		if _, err := part.Write(body); err != nil {
			return err
		}
		if err := part.Close(); err != nil {
			return err
		}
	}
	for _, section := range options.BinarySection {
		body := goimapserver.ExtractBodySection(bytes.NewReader(raw), &imap.FetchItemBodySection{Part: section.Part, Partial: section.Partial, Peek: section.Peek})
		part := writer.WriteBinarySection(section, int64(len(body)))
		if _, err := part.Write(body); err != nil {
			return err
		}
		if err := part.Close(); err != nil {
			return err
		}
	}
	for _, section := range options.BinarySectionSize {
		body := goimapserver.ExtractBodySection(bytes.NewReader(raw), &imap.FetchItemBodySection{Part: section.Part})
		writer.WriteBinarySectionSize(section, uint32(len(body)))
	}
	return nil
}

func messageHeader(raw []byte) textproto.Header {
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return textproto.Header{}
	}
	return header
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

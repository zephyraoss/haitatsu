package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
)

const fetchBatchSize = 500

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
	var (
		itemsByID map[string]*ent.MailboxMessage
		msgsByID  map[string]*ent.Message
		err       error
	)
	if needsItem {
		if itemsByID, err = s.loadMailboxMessages(ctx, selected); err != nil {
			return err
		}
	}
	if needsMessage {
		if msgsByID, err = s.loadMessages(ctx, selected); err != nil {
			return err
		}
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
			mm, ok := itemsByID[item.itemID]
			if !ok {
				_ = writer.Close()
				continue
			}
			writer.WriteInternalDate(mm.CreatedAt)
		}
		if needsMessage {
			msg, ok := msgsByID[item.messageID]
			if !ok {
				_ = writer.Close()
				continue
			}
			if options.RFC822Size {
				writer.WriteRFC822Size(msg.SizeBytes)
			}
			readBlob := needsBlob || (options.Envelope && len(msg.Headers) == 0)
			if options.Envelope && !readBlob {
				writer.WriteEnvelope(goimapserver.ExtractEnvelope(storedHeader(msg.Headers)))
			}
			if readBlob {
				raw, err := s.blobs.GetMessage(ctx, msg.BlobKey)
				if err != nil {
					return err
				}
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

func (s *session) loadMailboxMessages(ctx context.Context, selected []int) (map[string]*ent.MailboxMessage, error) {
	ids := make([]string, 0, len(selected))
	for _, index := range selected {
		if item := s.view.entries[index]; !item.gone {
			ids = append(ids, item.itemID)
		}
	}
	result := make(map[string]*ent.MailboxMessage, len(ids))
	for start := 0; start < len(ids); start += fetchBatchSize {
		rows, err := s.client.MailboxMessage.Query().Where(mailboxmessage.IDIn(ids[start:min(start+fetchBatchSize, len(ids))]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			result[row.ID] = row
		}
	}
	return result, nil
}

func (s *session) loadMessages(ctx context.Context, selected []int) (map[string]*ent.Message, error) {
	seen := make(map[string]struct{}, len(selected))
	ids := make([]string, 0, len(selected))
	for _, index := range selected {
		item := s.view.entries[index]
		if item.gone {
			continue
		}
		if _, ok := seen[item.messageID]; ok {
			continue
		}
		seen[item.messageID] = struct{}{}
		ids = append(ids, item.messageID)
	}
	result := make(map[string]*ent.Message, len(ids))
	for start := 0; start < len(ids); start += fetchBatchSize {
		rows, err := s.client.Message.Query().
			Where(message.IDIn(ids[start:min(start+fetchBatchSize, len(ids))]...)).
			Select(message.FieldID, message.FieldBlobKey, message.FieldSizeBytes, message.FieldHeaders).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			result[row.ID] = row
		}
	}
	return result, nil
}

func storedHeader(values map[string][]string) textproto.Header {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var header textproto.Header
	for _, key := range keys {
		for _, value := range values[key] {
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
	if options.Envelope {
		writer.WriteEnvelope(goimapserver.ExtractEnvelope(messageHeader(raw)))
	}
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

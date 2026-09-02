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
)

func (s *session) Fetch(w *goimapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if s.view == nil {
		return errNotSelected
	}
	ctx := context.Background()
	if err := s.resync(ctx, nil, syncMode{}); err != nil {
		return err
	}
	needsBlob := options.Envelope || options.BodyStructure != nil || len(options.BodySection) > 0 || len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0
	needsMessage := needsBlob || options.RFC822Size
	needsItem := options.InternalDate
	for _, index := range s.view.selected(numSet) {
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
		var mm *ent.MailboxMessage
		if needsItem {
			loaded, err := s.client.MailboxMessage.Get(ctx, item.itemID)
			if err != nil {
				if ent.IsNotFound(err) {
					_ = writer.Close()
					continue
				}
				return err
			}
			mm = loaded
			writer.WriteInternalDate(mm.CreatedAt)
		}
		if needsMessage {
			msg, err := s.client.Message.Get(ctx, item.messageID)
			if err != nil {
				if ent.IsNotFound(err) {
					_ = writer.Close()
					continue
				}
				return err
			}
			if options.RFC822Size {
				writer.WriteRFC822Size(msg.SizeBytes)
			}
			if needsBlob {
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

package mailparse

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

const extractLimit = 8192

type Metadata struct {
	RFCMessageID string
	Headers      map[string][]string
	From         []string
	To           []string
	CC           []string
	BCC          []string
	Subject      string
	Date         *time.Time
	TextExtract  string
	HTMLExtract  string
	Attachments  []map[string]any
}

type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

func Parse(data []byte) Metadata {
	reader, err := mail.CreateReader(bytes.NewReader(data))
	if err != nil && reader == nil {
		return Metadata{Headers: map[string][]string{}}
	}
	defer reader.Close()

	metadata := Metadata{
		RFCMessageID: headerMessageID(reader.Header),
		Headers:      headerMap(reader.Header.Header),
		From:         addressList(reader.Header, "From"),
		To:           addressList(reader.Header, "To"),
		CC:           addressList(reader.Header, "Cc"),
		BCC:          addressList(reader.Header, "Bcc"),
		Subject:      headerText(reader.Header, "Subject"),
	}
	if date, err := reader.Header.Date(); err == nil {
		metadata.Date = &date
	}

	partIndex := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || part == nil {
			break
		}
		partIndex++
		if attachment, ok := attachmentMetadata(part, partIndex); ok {
			size, _ := io.Copy(io.Discard, part.Body)
			attachment["size_bytes"] = size
			metadata.Attachments = append(metadata.Attachments, attachment)
			continue
		}
		contentType := partContentType(part)
		body := readExtract(part.Body)
		if contentType == "text/plain" && metadata.TextExtract == "" {
			metadata.TextExtract = body
		}
		if contentType == "text/html" && metadata.HTMLExtract == "" {
			metadata.HTMLExtract = body
		}
	}

	return metadata
}

func ExtractAttachment(data []byte, partIndex int) (Attachment, error) {
	reader, err := mail.CreateReader(bytes.NewReader(data))
	if err != nil && reader == nil {
		return Attachment{}, err
	}
	defer reader.Close()

	current := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || part == nil {
			return Attachment{}, err
		}
		current++
		if current != partIndex {
			_, _ = io.Copy(io.Discard, part.Body)
			continue
		}
		header, ok := part.Header.(*mail.AttachmentHeader)
		if !ok {
			return Attachment{}, fmt.Errorf("part is not an attachment")
		}
		filename, _ := header.Filename()
		contentType, _, _ := header.ContentType()
		body, err := io.ReadAll(part.Body)
		if err != nil {
			return Attachment{}, err
		}
		return Attachment{Filename: filename, ContentType: contentType, Data: body}, nil
	}
	return Attachment{}, fmt.Errorf("attachment not found")
}

func headerMap(header message.Header) map[string][]string {
	values := map[string][]string{}
	fields := header.Fields()
	for fields.Next() {
		values[fields.Key()] = append(values[fields.Key()], fields.Value())
	}
	return values
}

func headerMessageID(header mail.Header) string {
	messageID, err := header.MessageID()
	if err != nil {
		return ""
	}
	return messageID
}

func headerText(header mail.Header, key string) string {
	value, err := header.Text(key)
	if err != nil {
		return ""
	}
	return value
}

func addressList(header mail.Header, key string) []string {
	addresses, err := header.AddressList(key)
	if err != nil {
		return nil
	}
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, strings.ToLower(address.Address))
	}
	return values
}

func partContentType(part *mail.Part) string {
	inline, ok := part.Header.(*mail.InlineHeader)
	if !ok {
		return ""
	}
	contentType, _, err := inline.ContentType()
	if err != nil {
		return ""
	}
	return contentType
}

func attachmentMetadata(part *mail.Part, partIndex int) (map[string]any, bool) {
	header, ok := part.Header.(*mail.AttachmentHeader)
	if !ok {
		return nil, false
	}
	filename, _ := header.Filename()
	contentType, _, _ := header.ContentType()
	return map[string]any{
		"part_index":   partIndex,
		"filename":     filename,
		"content_type": contentType,
	}, true
}

func readExtract(reader io.Reader) string {
	var buf bytes.Buffer
	_, err := io.CopyN(&buf, reader, extractLimit)
	if err != nil && err != io.EOF {
		_, _ = io.Copy(io.Discard, reader)
		return ""
	}
	_, _ = io.Copy(io.Discard, reader)
	data := buf.Bytes()
	if err != nil {
		return string(data)
	}
	return string(data)
}

package mailparse

import (
	"bytes"
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

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || part == nil {
			break
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

func readExtract(reader io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(reader, extractLimit))
	if err != nil {
		return ""
	}
	return string(data)
}

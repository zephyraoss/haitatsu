package bounce

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

type Store interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type Handler struct {
	client  *ent.Client
	store   Store
	metrics *metrics.Metrics
	domain  string
}

type Recipient struct {
	Address   string
	MessageID string
}

func NewHandler(client *ent.Client, store Store, metrics *metrics.Metrics, domain string) *Handler {
	return &Handler{client: client, store: store, metrics: metrics, domain: strings.ToLower(domain)}
}

func (h *Handler) ParseRecipient(address string) (Recipient, bool, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !ok || domain != h.domain {
		return Recipient{}, false, false
	}
	messageID, ok := strings.CutPrefix(local, "bounce+")
	if !ok {
		return Recipient{}, true, false
	}
	if _, err := ulid.Parse(messageID); err != nil {
		return Recipient{}, true, false
	}
	return Recipient{Address: address, MessageID: messageID}, true, true
}

func (h *Handler) Record(ctx context.Context, recipient Recipient, raw []byte) error {
	key := fmt.Sprintf("bounces/%s/%s.eml", recipient.MessageID, ids.New().String())
	if err := h.store.PutMessage(ctx, key, raw); err != nil {
		return err
	}
	_, err := h.client.BounceEvent.Create().
		SetMessageID(recipient.MessageID).
		SetRecipient(recipient.Address).
		SetBlobKey(key).
		SetSha256(sha256Hex(raw)).
		SetSizeBytes(int64(len(raw))).
		SetDetails(bounceDetails(raw)).
		Save(ctx)
	if err == nil {
		h.metrics.MessageBounced()
	}
	return err
}

func bounceDetails(raw []byte) map[string]any {
	details := map[string]any{"received_at": time.Now().UTC().Format(time.RFC3339)}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return details
	}
	contentType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(contentType, "multipart/") || params["boundary"] == "" {
		return details
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return details
		}
		if partContentType(part) != "message/delivery-status" {
			continue
		}
		status, err := io.ReadAll(part)
		if err != nil {
			return details
		}
		mergeDetails(details, parseDeliveryStatus(status))
		return details
	}
	return details
}

func parseDeliveryStatus(data []byte) map[string]any {
	blocks := deliveryStatusBlocks(data)
	if len(blocks) == 0 {
		return nil
	}
	details := map[string]any{"dsn": true}
	setHeaderDetails(details, blocks[0], map[string]string{
		"Reporting-MTA":     "reporting_mta",
		"Arrival-Date":      "arrival_date",
		"Received-From-MTA": "received_from_mta",
	})
	if len(blocks) == 1 {
		return details
	}
	recipients := make([]map[string]any, 0, len(blocks)-1)
	for _, block := range blocks[1:] {
		recipient := map[string]any{}
		setHeaderDetails(recipient, block, map[string]string{
			"Original-Recipient": "original_recipient",
			"Final-Recipient":    "final_recipient",
			"Action":             "action",
			"Status":             "status",
			"Remote-MTA":         "remote_mta",
			"Diagnostic-Code":    "diagnostic_code",
			"Last-Attempt-Date":  "last_attempt_date",
			"Will-Retry-Until":   "will_retry_until",
		})
		if finalRecipient, _ := recipient["final_recipient"].(string); finalRecipient != "" {
			recipient["final_recipient_address"] = statusAddress(finalRecipient)
		}
		if len(recipient) > 0 {
			recipients = append(recipients, recipient)
		}
	}
	if len(recipients) > 0 {
		details["recipients"] = recipients
	}
	return details
}

func deliveryStatusBlocks(data []byte) []textproto.MIMEHeader {
	var blocks []textproto.MIMEHeader
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, block := range strings.Split(normalized, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		reader := textproto.NewReader(bufio.NewReader(strings.NewReader(block + "\r\n\r\n")))
		header, err := reader.ReadMIMEHeader()
		if err != nil || len(header) == 0 {
			continue
		}
		if len(header) == 0 {
			continue
		}
		blocks = append(blocks, header)
	}
	return blocks
}

func setHeaderDetails(target map[string]any, header textproto.MIMEHeader, fields map[string]string) {
	for headerName, detailName := range fields {
		if value := header.Get(headerName); value != "" {
			target[detailName] = value
		}
	}
}

func mergeDetails(target map[string]any, values map[string]any) {
	for key, value := range values {
		target[key] = value
	}
}

func statusAddress(value string) string {
	_, address, ok := strings.Cut(value, ";")
	if !ok {
		return strings.TrimSpace(value)
	}
	return strings.Trim(strings.TrimSpace(address), "<>")
}

func partContentType(part *multipart.Part) string {
	contentType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return strings.ToLower(contentType)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

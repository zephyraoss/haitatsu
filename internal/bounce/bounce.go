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
	"github.com/zephyraoss/haitatsu/internal/database/ent/dkimkey"
	"github.com/zephyraoss/haitatsu/internal/mailaddr"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

const (
	verpPrefix          = "bounces+"
	maxStatusBlocks     = 64
	maxStatusRecipients = 32
)

type Store interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type hostedDomainChecker interface {
	IsHosted(ctx context.Context, domain string) (bool, error)
}

type dkimHostedDomains struct {
	client *ent.Client
}

func (d dkimHostedDomains) IsHosted(ctx context.Context, domain string) (bool, error) {
	return d.client.DKIMKey.Query().Where(dkimkey.DomainEQ(domain)).Exist(ctx)
}

type Handler struct {
	client  *ent.Client
	store   Store
	metrics *metrics.Metrics
	hosted  hostedDomainChecker
}

type Recipient struct {
	Address   string
	MessageID string
}

func NewHandler(client *ent.Client, store Store, metrics *metrics.Metrics) *Handler {
	return &Handler{
		client:  client,
		store:   store,
		metrics: metrics,
		hosted:  dkimHostedDomains{client: client},
	}
}

func (h *Handler) ParseRecipient(ctx context.Context, address string) (Recipient, bool, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !ok {
		return Recipient{}, false, false
	}
	if !strings.HasPrefix(local, verpPrefix) {
		if mailaddr.ReservedLocalPart(local) {
			return Recipient{}, true, false
		}
		return Recipient{}, false, false
	}
	messageID, ok := strings.CutPrefix(local, verpPrefix)
	if !ok {
		return Recipient{}, true, false
	}
	parsed, err := ulid.Parse(messageID)
	if err != nil {
		return Recipient{}, true, false
	}
	hosted, err := h.hosted.IsHosted(ctx, domain)
	if err != nil || !hosted {
		return Recipient{}, true, false
	}
	return Recipient{Address: address, MessageID: parsed.String()}, true, true
}

func (h *Handler) Record(ctx context.Context, recipients []Recipient, raw []byte) error {
	recipients = uniqueRecipients(recipients)
	if len(recipients) == 0 {
		return nil
	}
	sum := sha256Hex(raw)
	key := fmt.Sprintf("bounces/sha256/%s.eml", sum)
	if err := h.store.PutMessage(ctx, key, raw); err != nil {
		return err
	}
	details := bounceDetails(raw)
	creates := make([]*ent.BounceEventCreate, 0, len(recipients))
	for _, recipient := range recipients {
		creates = append(creates, h.client.BounceEvent.Create().
			SetMessageID(recipient.MessageID).
			SetRecipient(recipient.Address).
			SetBlobKey(key).
			SetSha256(sum).
			SetSizeBytes(int64(len(raw))).
			SetDetails(details))
	}
	if _, err := h.client.BounceEvent.CreateBulk(creates...).Save(ctx); err != nil {
		return err
	}
	for range creates {
		h.metrics.MessageBounced()
	}
	return nil
}

func uniqueRecipients(recipients []Recipient) []Recipient {
	seen := make(map[string]struct{}, len(recipients))
	unique := make([]Recipient, 0, len(recipients))
	for _, recipient := range recipients {
		if _, ok := seen[recipient.MessageID]; ok {
			continue
		}
		seen[recipient.MessageID] = struct{}{}
		unique = append(unique, recipient)
	}
	return unique
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
	for parts := 0; parts < mailparse.MaxParts; parts++ {
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
		status, err := io.ReadAll(io.LimitReader(part, mailparse.MaxStatusBytes))
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
		if len(recipients) >= maxStatusRecipients {
			break
		}
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
	for len(normalized) > 0 && len(blocks) < maxStatusBlocks {
		block, rest, _ := strings.Cut(normalized, "\n\n")
		normalized = rest
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		reader := textproto.NewReader(bufio.NewReader(strings.NewReader(block + "\r\n\r\n")))
		header, err := reader.ReadMIMEHeader()
		if err != nil || len(header) == 0 {
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

package outbound

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/dkimkey"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/outboundjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/route"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

var ErrDKIMKeyNotFound = errors.New("dkim key not found")
var ErrSenderNotAllowed = errors.New("sender not allowed")
var ErrRateLimited = errors.New("outbound rate limit exceeded")
var ErrTooManyRecipients = errors.New("too many recipients")
var ErrOverQuota = mailstore.ErrOverQuota

type BlobStore interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type Limits struct {
	PerHour              int64
	PerDay               int64
	RecipientsPerMessage int64
}

type Submission struct {
	client         *ent.Client
	blobs          BlobStore
	store          *mailstore.Store
	publicHostname string
	instanceName   string
	defaults       func() Limits
}

func NewSubmission(client *ent.Client, blobs BlobStore, store *mailstore.Store, publicHostname string, instanceName string, defaults func() Limits) *Submission {
	if defaults == nil {
		defaults = func() Limits { return Limits{} }
	}
	return &Submission{client: client, blobs: blobs, store: store, publicHostname: publicHostname, instanceName: instanceName, defaults: defaults}
}

func (s *Submission) SenderAllowed(ctx context.Context, mbox *ent.Mailbox, address string) (bool, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if strings.EqualFold(address, mbox.PrimaryAddress) {
		return true, nil
	}
	routes, err := s.client.Route.Query().Where(route.SourceAddressEqualFold(address), route.StatusEQ("active"), route.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range routes {
		for _, destination := range item.Destinations {
			if destination == mbox.ID {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Submission) Submit(ctx context.Context, mailboxID string, from string, raw []byte, recipients []string) (*ent.Message, error) {
	sender, err := mail.ParseAddress(from)
	if err != nil {
		return nil, err
	}
	mbox, err := s.client.Mailbox.Query().Where(mailbox.IDEQ(mailboxID), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		return nil, err
	}
	allowed, err := s.SenderAllowed(ctx, mbox, sender.Address)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrSenderNotAllowed
	}
	domain := addressDomain(sender.Address)
	key, err := s.client.DKIMKey.Query().Where(dkimkey.DomainEQ(domain)).First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrDKIMKeyNotFound
		}
		return nil, err
	}

	messageID := ids.New().String()
	traceID := ids.New().String()
	recipients = outboundRecipients(recipients, mailparse.Parse(mailparse.NormalizeMessage(raw)))
	normalized := normalizeSubmittedMessage(raw, sender.Address, messageID, s.publicHostname, traceID, s.instanceName)
	if err := s.enforceLimits(ctx, mbox, len(recipients)); err != nil {
		return nil, err
	}
	if mailstore.OverQuotaWith(mbox, int64(len(normalized))) {
		return nil, ErrOverQuota
	}
	signed, err := signMessage(normalized, key)
	if err != nil {
		return nil, err
	}

	objectKey := messageObjectKey(time.Now().UTC(), messageID)
	if err := s.blobs.PutMessage(ctx, objectKey, signed); err != nil {
		return nil, err
	}
	msg, err := s.createMessage(ctx, messageID, traceID, objectKey, signed, mailparse.Parse(signed))
	if err != nil {
		return nil, err
	}
	if err := s.saveToSent(ctx, mailboxID, msg.ID, int64(len(signed))); err != nil {
		return nil, err
	}
	if _, err := s.client.OutboundJob.Create().SetMailboxID(mailboxID).SetMessageID(msg.ID).SetReturnPath(ReturnPath(msg.ID, domain)).SetRecipients(recipients).Save(ctx); err != nil {
		return nil, err
	}
	return msg, nil
}

func (s *Submission) enforceLimits(ctx context.Context, mbox *ent.Mailbox, recipientCount int) error {
	limits := s.limitsFor(mbox)
	if limits.RecipientsPerMessage > 0 && int64(recipientCount) > limits.RecipientsPerMessage {
		return ErrTooManyRecipients
	}
	if limits.PerHour > 0 {
		count, err := s.client.OutboundJob.Query().Where(outboundjob.MailboxIDEQ(mbox.ID), outboundjob.CreatedAtGTE(time.Now().Add(-time.Hour))).Count(ctx)
		if err != nil {
			return err
		}
		if int64(count) >= limits.PerHour {
			return ErrRateLimited
		}
	}
	if limits.PerDay > 0 {
		count, err := s.client.OutboundJob.Query().Where(outboundjob.MailboxIDEQ(mbox.ID), outboundjob.CreatedAtGTE(time.Now().Add(-24*time.Hour))).Count(ctx)
		if err != nil {
			return err
		}
		if int64(count) >= limits.PerDay {
			return ErrRateLimited
		}
	}
	return nil
}

func (s *Submission) limitsFor(mbox *ent.Mailbox) Limits {
	limits := s.defaults()
	if value, ok := mbox.OutboundLimits["per_hour"]; ok {
		limits.PerHour = value
	}
	if value, ok := mbox.OutboundLimits["per_day"]; ok {
		limits.PerDay = value
	}
	if value, ok := mbox.OutboundLimits["recipients_per_message"]; ok {
		limits.RecipientsPerMessage = value
	}
	return limits
}

func outboundRecipients(recipients []string, metadata mailparse.Metadata) []string {
	if len(recipients) > 0 {
		return dedupeAddresses(recipients)
	}
	values := make([]string, 0, len(metadata.To)+len(metadata.CC)+len(metadata.BCC))
	values = append(values, metadata.To...)
	values = append(values, metadata.CC...)
	values = append(values, metadata.BCC...)
	return dedupeAddresses(values)
}

func dedupeAddresses(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		address := value
		if parsed, err := mail.ParseAddress(value); err == nil {
			address = parsed.Address
		}
		key := strings.ToLower(strings.TrimSpace(address))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, address)
	}
	return result
}

func ReturnPath(messageID, domain string) string {
	return "bounces+" + messageID + "@" + domain
}

func (s *Submission) createMessage(ctx context.Context, id string, traceID string, key string, raw []byte, metadata mailparse.Metadata) (*ent.Message, error) {
	create := s.client.Message.Create().
		SetID(id).
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
	return create.Save(ctx)
}

func (s *Submission) saveToSent(ctx context.Context, mailboxID string, messageID string, size int64) error {
	sent, err := s.store.FolderByName(ctx, mailboxID, "Sent")
	if err != nil {
		return err
	}
	_, err = s.store.Attach(ctx, mailstore.Attach{MailboxID: mailboxID, MessageID: messageID, FolderID: sent.ID, SizeBytes: size, Flags: mailstore.Flags{Seen: true}})
	return err
}

func signMessage(raw []byte, key *ent.DKIMKey) ([]byte, error) {
	signer, err := parsePrivateKey(key.PrivateKeyPem)
	if err != nil {
		return nil, err
	}
	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:                 key.Domain,
		Selector:               key.Selector,
		Signer:                 signer,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys: []string{
			"From", "To", "Cc", "Subject", "Date", "Message-ID", "Reply-To", "In-Reply-To", "References", "MIME-Version", "Content-Type", "X-Haitatsu-Trace-ID", "X-Haitatsu-Node",
		},
	})
	if err != nil {
		return nil, err
	}
	return signed.Bytes(), nil
}

func parsePrivateKey(value string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, fmt.Errorf("invalid private key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func normalizeSubmittedMessage(raw []byte, from string, messageID string, hostname string, traceID string, node string) []byte {
	normalized, err := mailparse.InjectHeaders(raw, func(h textproto.MIMEHeader) {
		if h.Get("From") == "" {
			h.Set("From", from)
		}
		if h.Get("Date") == "" {
			h.Set("Date", time.Now().UTC().Format(time.RFC1123Z))
		}
		if h.Get("Message-ID") == "" {
			h.Set("Message-ID", "<"+messageID+"@"+hostname+">")
		}
		h.Del("Bcc")
		h.Set("X-Haitatsu-Trace-ID", traceID)
		h.Set("X-Haitatsu-Node", node)
	})
	if err == nil {
		return normalized
	}
	return normalizeSubmittedMessageFallback(raw, from, messageID, hostname, traceID, node)
}

func normalizeSubmittedMessageFallback(raw []byte, from string, messageID string, hostname string, traceID string, node string) []byte {
	header, body := mailparse.SplitHeaderBody(raw)
	body = mailparse.StripHaitatsuHeadersFromBody(body)
	header = mailparse.RemoveHeaderField(header, "Bcc")
	var extra [][]byte
	if !hasHeader(header, "From") {
		extra = append(extra, []byte("From: "+from))
	}
	if !hasHeader(header, "Date") {
		extra = append(extra, []byte("Date: "+time.Now().UTC().Format(time.RFC1123Z)))
	}
	if !hasHeader(header, "Message-ID") {
		extra = append(extra, []byte("Message-ID: <"+messageID+"@"+hostname+">"))
	}
	extra = append(extra,
		[]byte("X-Haitatsu-Trace-ID: "+traceID),
		[]byte("X-Haitatsu-Node: "+node),
	)
	header = mailparse.AppendHeaderFields(header, extra...)
	return mailparse.JoinHeaderBody(header, body)
}

func hasHeader(header []byte, key string) bool {
	prefix := strings.ToLower(key) + ":"
	for _, line := range strings.Split(string(header), "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), prefix) {
			return true
		}
	}
	return false
}

func messageObjectKey(t time.Time, messageID string) string {
	return fmt.Sprintf("messages/%04d/%02d/%02d/%s.eml", t.Year(), t.Month(), t.Day(), messageID)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func addressDomain(address string) string {
	_, domain, _ := strings.Cut(strings.ToLower(address), "@")
	return domain
}

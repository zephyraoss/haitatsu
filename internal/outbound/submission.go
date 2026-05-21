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
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/dkimkey"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

var ErrDKIMKeyNotFound = errors.New("dkim key not found")
var ErrSenderNotAllowed = errors.New("sender not allowed")

type BlobStore interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type Submission struct {
	client         *ent.Client
	store          BlobStore
	publicHostname string
	instanceName   string
	bounceDomain   string
}

func NewSubmission(client *ent.Client, store BlobStore, publicHostname string, instanceName string, bounceDomain string) *Submission {
	return &Submission{client: client, store: store, publicHostname: publicHostname, instanceName: instanceName, bounceDomain: bounceDomain}
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
	if !strings.EqualFold(sender.Address, mbox.PrimaryAddress) {
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
	normalized := normalizeSubmittedMessage(raw, sender.Address, messageID, s.publicHostname, traceID, s.instanceName)
	signed, err := signMessage(normalized, key)
	if err != nil {
		return nil, err
	}

	objectKey := messageObjectKey(time.Now().UTC(), messageID)
	if err := s.store.PutMessage(ctx, objectKey, signed); err != nil {
		return nil, err
	}
	metadata := mailparse.Parse(signed)
	recipients = outboundRecipients(recipients, metadata)
	msg, err := s.createMessage(ctx, messageID, traceID, objectKey, signed, metadata)
	if err != nil {
		return nil, err
	}
	if err := s.saveToSent(ctx, mailboxID, msg.ID, signed); err != nil {
		return nil, err
	}
	if _, err := s.client.OutboundJob.Create().SetMailboxID(mailboxID).SetMessageID(msg.ID).SetReturnPath(ReturnPath(msg.ID, s.bounceDomain)).SetRecipients(recipients).Save(ctx); err != nil {
		return nil, err
	}
	return msg, nil
}

func outboundRecipients(recipients []string, metadata mailparse.Metadata) []string {
	if len(recipients) > 0 {
		return recipients
	}
	values := make([]string, 0, len(metadata.To)+len(metadata.CC)+len(metadata.BCC))
	values = append(values, metadata.To...)
	values = append(values, metadata.CC...)
	values = append(values, metadata.BCC...)
	return values
}

func ReturnPath(messageID string, bounceDomain string) string {
	return "bounce+" + messageID + "@" + bounceDomain
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

func (s *Submission) saveToSent(ctx context.Context, mailboxID string, messageID string, raw []byte) error {
	sent, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(mailboxID), folder.NameEQ("Sent")).Only(ctx)
	if err != nil {
		return err
	}
	if _, err := s.client.MailboxMessage.Create().SetMailboxID(mailboxID).SetMessageID(messageID).SetFolderID(sent.ID).SetOriginalRcpt("").SetBaseRcpt("").Save(ctx); err != nil {
		return err
	}
	_, err = s.client.Mailbox.UpdateOneID(mailboxID).AddUsedBytes(int64(len(raw))).Save(ctx)
	return err
}

func signMessage(raw []byte, key *ent.DKIMKey) ([]byte, error) {
	signer, err := parsePrivateKey(key.PrivateKeyPem)
	if err != nil {
		return nil, err
	}
	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:   key.Domain,
		Selector: key.Selector,
		Signer:   signer,
		Hash:     crypto.SHA256,
		HeaderKeys: []string{
			"From", "To", "Cc", "Subject", "Date", "Message-ID", "X-Haitatsu-Trace-ID", "X-Haitatsu-Node",
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
	header, body := mailparse.SplitHeaderBody(raw)
	header = mailparse.JoinHeaderBody(header, nil)
	if !hasHeader(header, "From") {
		header = append(header, []byte("From: "+from+"\r\n")...)
	}
	if !hasHeader(header, "Date") {
		header = append(header, []byte("Date: "+time.Now().UTC().Format(time.RFC1123Z)+"\r\n")...)
	}
	if !hasHeader(header, "Message-ID") {
		header = append(header, []byte("Message-ID: <"+messageID+"@"+hostname+">\r\n")...)
	}
	header = append(header, []byte("X-Haitatsu-Trace-ID: "+traceID+"\r\n")...)
	header = append(header, []byte("X-Haitatsu-Node: "+node+"\r\n")...)
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

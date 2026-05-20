package messages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/rules"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

type BlobStore interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type EventSink interface {
	EmitMessageReceived(ctx context.Context, msg *ent.Message, recipients []routing.Result) error
}

type Service struct {
	client         *ent.Client
	store          BlobStore
	events         EventSink
	rules          *rules.Engine
	metrics        *metrics.Metrics
	publicHostname string
	instanceName   string
}

func NewService(client *ent.Client, store BlobStore, events EventSink, rules *rules.Engine, metrics *metrics.Metrics, publicHostname string, instanceName string) *Service {
	return &Service{client: client, store: store, events: events, rules: rules, metrics: metrics, publicHostname: publicHostname, instanceName: instanceName}
}

func (s *Service) Deliver(ctx context.Context, raw []byte, recipients []routing.Result, assessment spam.Assessment) (*ent.Message, error) {
	messageID := ids.New().String()
	traceID := ids.New().String()
	key := messageObjectKey(time.Now().UTC(), messageID)
	stored := s.withTraceHeaders(raw, traceID, assessment.Header)
	metadata := mailparse.Parse(stored)

	if err := s.store.PutMessage(ctx, key, stored); err != nil {
		return nil, err
	}

	messageCreate := s.client.Message.Create().
		SetID(messageID).
		SetTraceID(traceID).
		SetBlobKey(key).
		SetSha256(sha256Hex(stored)).
		SetSizeBytes(int64(len(stored))).
		SetHeaders(metadata.Headers).
		SetFromAddresses(metadata.From).
		SetToAddresses(metadata.To).
		SetCcAddresses(metadata.CC).
		SetBccAddresses(metadata.BCC).
		SetSubject(metadata.Subject).
		SetTextBodyExtract(metadata.TextExtract).
		SetHTMLBodyExtract(metadata.HTMLExtract).
		SetAttachments(metadata.Attachments).
		SetSpamScore(assessment.Score).
		SetAuthResults(assessment.AuthResults)
	if metadata.RFCMessageID != "" {
		messageCreate.SetRfcMessageID(metadata.RFCMessageID)
	}
	if metadata.Date != nil {
		messageCreate.SetDate(*metadata.Date)
	}

	message, err := messageCreate.Save(ctx)
	if err != nil {
		return nil, err
	}
	deliveries, err := s.createMailboxMessages(ctx, message.ID, stored, recipients, assessment)
	if err != nil {
		return nil, err
	}
	s.metrics.MessageReceived()
	s.metrics.MessageDelivered(len(deliveries))
	if s.rules != nil {
		if err := s.rules.Apply(ctx, message, deliveries); err != nil {
			return nil, err
		}
	}
	if s.events != nil {
		if err := s.events.EmitMessageReceived(ctx, message, recipients); err != nil {
			return nil, err
		}
	}
	return message, nil
}

func (s *Service) createMailboxMessages(ctx context.Context, messageID string, raw []byte, recipients []routing.Result, assessment spam.Assessment) ([]rules.Delivery, error) {
	seen := map[string]struct{}{}
	var deliveries []rules.Delivery
	for _, recipient := range recipients {
		for _, mbox := range recipient.Mailboxes {
			if _, ok := seen[mbox.ID]; ok {
				continue
			}
			seen[mbox.ID] = struct{}{}

			folderName := "INBOX"
			if assessment.Junk {
				folderName = "Junk"
			}
			deliveryFolder, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(mbox.ID), folder.NameEQ(folderName)).Only(ctx)
			if err != nil {
				return nil, err
			}

			create := s.client.MailboxMessage.Create().
				SetMailboxID(mbox.ID).
				SetMessageID(messageID).
				SetFolderID(deliveryFolder.ID).
				SetOriginalRcpt(recipient.OriginalRecipient).
				SetBaseRcpt(recipient.BaseRecipient)
			if recipient.PlusTag != "" {
				create.SetPlusTag(recipient.PlusTag)
			}
			if recipient.RouteID != "" {
				create.SetResolvedRouteID(recipient.RouteID)
			}
			item, err := create.Save(ctx)
			if err != nil {
				return nil, err
			}
			deliveries = append(deliveries, rules.Delivery{Message: item, Route: recipient})
			if _, err := s.client.Mailbox.UpdateOneID(mbox.ID).AddUsedBytes(int64(len(raw))).Save(ctx); err != nil {
				return nil, err
			}
		}
	}
	return deliveries, nil
}

func messageObjectKey(t time.Time, messageID string) string {
	return fmt.Sprintf("messages/%04d/%02d/%02d/%s.eml", t.Year(), t.Month(), t.Day(), messageID)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Service) withTraceHeaders(raw []byte, traceID string, authResults string) []byte {
	stamp := time.Now().UTC().Format(time.RFC1123Z)
	if authResults == "" {
		authResults = s.publicHostname + "; none"
	}
	headers := fmt.Sprintf("Received: by %s (Haitatsu/0.1; %s) with SMTP; %s\r\nAuthentication-Results: %s\r\nX-Haitatsu-Trace-ID: %s\r\nX-Haitatsu-Node: %s\r\n", s.publicHostname, s.instanceName, stamp, authResults, traceID, s.instanceName)
	return append([]byte(headers), raw...)
}

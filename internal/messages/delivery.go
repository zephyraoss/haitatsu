package messages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/rules"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

var ErrOverQuota = mailstore.ErrOverQuota

type BlobStore interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type EventSink interface {
	EmitMessageReceived(ctx context.Context, msg *ent.Message, recipients []routing.Result) error
}

type Service struct {
	client         *ent.Client
	blobs          BlobStore
	store          *mailstore.Store
	events         EventSink
	rules          *rules.Engine
	metrics        *metrics.Metrics
	publicHostname string
	instanceName   string
}

func NewService(client *ent.Client, blobs BlobStore, store *mailstore.Store, events EventSink, rules *rules.Engine, metrics *metrics.Metrics, publicHostname string, instanceName string) *Service {
	return &Service{client: client, blobs: blobs, store: store, events: events, rules: rules, metrics: metrics, publicHostname: publicHostname, instanceName: instanceName}
}

func (s *Service) Deliver(ctx context.Context, raw []byte, recipients []routing.Result, assessment spam.Assessment) (*ent.Message, error) {
	messageID := ids.New().String()
	traceID := ids.New().String()
	stored := s.withTraceHeaders(mailparse.NormalizeMessage(raw), traceID, assessment.Header)
	return s.deliver(ctx, messageID, traceID, stored, mailparse.Parse(stored), recipients, assessment)
}

// DeliverParsed is Deliver for a message that is already normalized and
// parsed; only the header map is re-read after trace headers are prepended.
func (s *Service) DeliverParsed(ctx context.Context, normalized []byte, metadata mailparse.Metadata, recipients []routing.Result, assessment spam.Assessment) (*ent.Message, error) {
	messageID := ids.New().String()
	traceID := ids.New().String()
	stored := s.withTraceHeaders(normalized, traceID, assessment.Header)
	metadata.Headers = mailparse.ParseHeaders(stored)
	return s.deliver(ctx, messageID, traceID, stored, metadata, recipients, assessment)
}

func (s *Service) deliver(ctx context.Context, messageID string, traceID string, stored []byte, metadata mailparse.Metadata, recipients []routing.Result, assessment spam.Assessment) (*ent.Message, error) {
	key := messageObjectKey(time.Now().UTC(), messageID)

	pending, err := s.pendingDeliveries(ctx, messageID, int64(len(stored)), recipients, assessment)
	if err != nil {
		return nil, err
	}
	if err := s.blobs.PutMessage(ctx, key, stored); err != nil {
		return nil, err
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	messageCreate := tx.Client().Message.Create().
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
		_ = tx.Rollback()
		return nil, err
	}
	deliveries, err := s.attachInTx(ctx, tx, pending)
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, mailstore.ErrOverQuota) {
			s.metrics.MailboxOverQuota()
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i, delivery := range deliveries {
		s.store.NotifyAttached(ctx, pending[i].attach, delivery.Message)
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

type pendingDelivery struct {
	attach mailstore.Attach
	route  routing.Result
}

func (s *Service) pendingDeliveries(ctx context.Context, messageID string, size int64, recipients []routing.Result, assessment spam.Assessment) ([]pendingDelivery, error) {
	seen := map[string]struct{}{}
	var pending []pendingDelivery
	for _, recipient := range recipients {
		for _, mbox := range recipient.Mailboxes {
			if _, ok := seen[mbox.ID]; ok {
				continue
			}
			seen[mbox.ID] = struct{}{}

			folderName := "INBOX"
			if assessment.JunkForMailbox(mbox.ID) {
				folderName = "Junk"
			}
			deliveryFolder, err := s.store.FolderByName(ctx, mbox.ID, folderName)
			if err != nil {
				return nil, err
			}
			pending = append(pending, pendingDelivery{route: recipient, attach: mailstore.Attach{
				MailboxID:       mbox.ID,
				MessageID:       messageID,
				FolderID:        deliveryFolder.ID,
				SizeBytes:       size,
				OriginalRcpt:    recipient.OriginalRecipient,
				BaseRcpt:        recipient.BaseRecipient,
				PlusTag:         recipient.PlusTag,
				ResolvedRouteID: recipient.RouteID,
				EnforceQuota:    true,
			}})
		}
	}
	return pending, nil
}

func (s *Service) attachInTx(ctx context.Context, tx *ent.Tx, pending []pendingDelivery) ([]rules.Delivery, error) {
	deliveries := make([]rules.Delivery, 0, len(pending))
	for _, p := range pending {
		item, err := s.store.AttachInTx(ctx, tx, p.attach)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, rules.Delivery{Message: item, Route: p.route})
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

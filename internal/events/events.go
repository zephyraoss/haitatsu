package events

import (
	"context"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

const MessageReceived = "message.received"

type Service struct {
	client *ent.Client
}

func New(client *ent.Client) *Service {
	return &Service{client: client}
}

func (s *Service) EmitMessageReceived(ctx context.Context, msg *ent.Message, recipients []routing.Result) error {
	for _, recipient := range recipients {
		mailboxIDs := deliveredMailboxIDs(recipient.Mailboxes)
		payload := map[string]any{
			"event":                MessageReceived,
			"routing_rule_id":      "",
			"trace_id":             msg.TraceID,
			"message_id":           msg.ID,
			"original_rcpt":        recipient.OriginalRecipient,
			"base_rcpt":            recipient.BaseRecipient,
			"plus_tag":             recipient.PlusTag,
			"default_destinations": mailboxIDs,
		}
		create := s.client.EventLog.Create().SetEventType(MessageReceived).SetTraceID(msg.TraceID).SetMessageID(msg.ID).SetPayload(payload)
		if len(mailboxIDs) == 1 {
			create.SetMailboxID(mailboxIDs[0])
		}
		if _, err := create.Save(ctx); err != nil {
			return err
		}
	}
	return nil
}

func deliveredMailboxIDs(mailboxes []*ent.Mailbox) []string {
	ids := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		ids = append(ids, mailbox.ID)
	}
	return ids
}

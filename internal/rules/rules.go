package rules

import (
	"context"
	"errors"
	"strings"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/routingrule"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

type Engine struct {
	client *ent.Client
	store  *mailstore.Store
	events EventSink
}

type EventSink interface {
	Emit(ctx context.Context, eventType string, mailboxID string, payload map[string]any) error
}

type Delivery struct {
	Message *ent.MailboxMessage
	Route   routing.Result
}

func New(client *ent.Client, store *mailstore.Store, events ...EventSink) *Engine {
	engine := &Engine{client: client, store: store}
	if len(events) > 0 {
		engine.events = events[0]
	}
	return engine
}

func (e *Engine) Apply(ctx context.Context, msg *ent.Message, deliveries []Delivery) error {
	for _, delivery := range deliveries {
		if err := e.applyDelivery(ctx, msg, delivery); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) applyDelivery(ctx context.Context, msg *ent.Message, delivery Delivery) error {
	rules, err := e.rules(ctx, msg, delivery)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if !conditionsMatch(rule.Conditions, msg, delivery.Route) {
			continue
		}
		for _, action := range rule.Actions {
			if err := e.applyAction(ctx, msg, rule, delivery, action); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) rules(ctx context.Context, msg *ent.Message, delivery Delivery) ([]*ent.RoutingRule, error) {
	var rules []*ent.RoutingRule
	load := func(scope string, refs ...string) error {
		query := e.client.RoutingRule.Query().Where(routingrule.ScopeEQ(scope), routingrule.EnabledEQ(true))
		if len(refs) > 0 {
			query.Where(routingrule.ScopeRefIn(refs...))
		}
		items, err := query.Order(routingrule.ByPriority(), routingrule.ByCreatedAt(entsql.OrderAsc())).All(ctx)
		if err != nil {
			return err
		}
		rules = append(rules, items...)
		return nil
	}

	if delivery.Route.RouteID != "" {
		if err := load("route", delivery.Route.RouteID); err != nil {
			return nil, err
		}
	}
	if err := load("address", delivery.Route.OriginalRecipient, delivery.Route.BaseRecipient); err != nil {
		return nil, err
	}
	if err := load("mailbox", delivery.Message.MailboxID); err != nil {
		return nil, err
	}
	if domain := firstDomain(msg.FromAddresses); domain != "" {
		if err := load("domain", domain); err != nil {
			return nil, err
		}
	}
	if err := load("global"); err != nil {
		return nil, err
	}
	return rules, nil
}

func (e *Engine) applyAction(ctx context.Context, msg *ent.Message, rule *ent.RoutingRule, delivery Delivery, action map[string]any) error {
	item := delivery.Message
	typeName := actionString(action, "type")
	if typeName == "" {
		typeName = actionString(action, "action")
	}
	switch typeName {
	case "move", "move_to_folder":
		return e.move(ctx, item, action)
	case "add_label", "label":
		return e.addLabel(ctx, item, action)
	case "mark_read":
		_, err := e.client.MailboxMessage.UpdateOneID(item.ID).SetRead(true).Save(ctx)
		return err
	case "mark_unread":
		_, err := e.client.MailboxMessage.UpdateOneID(item.ID).SetRead(false).Save(ctx)
		return err
	case "mark_flagged":
		_, err := e.client.MailboxMessage.UpdateOneID(item.ID).SetFlagged(true).Save(ctx)
		return err
	case "junk", "quarantine":
		return e.moveToFolder(ctx, item, "Junk")
	case "delete", "drop":
		return e.store.SoftDelete(ctx, item)
	case "copy_to_mailbox":
		return e.copyToMailbox(ctx, item, actionString(action, "mailbox_id"))
	case "webhook", "emit_webhook":
		return e.emitWebhook(ctx, msg, rule, delivery, action)
	default:
		return nil
	}
}

func (e *Engine) emitWebhook(ctx context.Context, msg *ent.Message, rule *ent.RoutingRule, delivery Delivery, action map[string]any) error {
	if e.events == nil {
		return nil
	}
	eventType := actionString(action, "event_type")
	if eventType == "" {
		eventType = actionString(action, "event")
	}
	if eventType == "" {
		eventType = "message.routed"
	}
	payload := webhookPayload(msg, rule, delivery, eventType)
	if custom, ok := action["payload"].(map[string]any); ok {
		for key, value := range custom {
			payload[key] = value
		}
	}
	return e.events.Emit(ctx, eventType, delivery.Message.MailboxID, payload)
}

func webhookPayload(msg *ent.Message, rule *ent.RoutingRule, delivery Delivery, eventType string) map[string]any {
	return map[string]any{
		"event":              eventType,
		"trace_id":           msg.TraceID,
		"message_id":         msg.ID,
		"mailbox_id":         delivery.Message.MailboxID,
		"mailbox_message_id": delivery.Message.ID,
		"routing_rule_id":    rule.ID,
		"routing_rule_name":  rule.Name,
		"original_rcpt":      delivery.Route.OriginalRecipient,
		"base_rcpt":          delivery.Route.BaseRecipient,
		"plus_tag":           delivery.Route.PlusTag,
		"route_id":           delivery.Route.RouteID,
	}
}

func (e *Engine) move(ctx context.Context, item *ent.MailboxMessage, action map[string]any) error {
	if folderID := actionString(action, "folder_id"); folderID != "" {
		_, err := e.store.Move(ctx, item, folderID)
		return err
	}
	folderName := actionString(action, "folder")
	if folderName == "" {
		folderName = actionString(action, "folder_name")
	}
	if folderName == "" {
		return nil
	}
	return e.moveToFolder(ctx, item, folderName)
}

func (e *Engine) moveToFolder(ctx context.Context, item *ent.MailboxMessage, folderName string) error {
	_, err := e.store.MoveToFolderName(ctx, item, folderName)
	return err
}

func (e *Engine) addLabel(ctx context.Context, item *ent.MailboxMessage, action map[string]any) error {
	labelID := actionString(action, "label_id")
	if labelID == "" {
		labelName := actionString(action, "label")
		if labelName == "" {
			labelName = actionString(action, "label_name")
		}
		if labelName == "" {
			return nil
		}
		labelItem, err := e.client.Label.Query().Where(label.MailboxIDEQ(item.MailboxID), label.NameEQ(labelName)).Only(ctx)
		if err != nil {
			return err
		}
		labelID = labelItem.ID
	}
	_, err := e.store.AddLabel(ctx, item, labelID)
	return err
}

func (e *Engine) copyToMailbox(ctx context.Context, item *ent.MailboxMessage, mailboxID string) error {
	if mailboxID == "" || mailboxID == item.MailboxID {
		return nil
	}
	inbox, err := e.store.FolderByName(ctx, mailboxID, "INBOX")
	if err != nil {
		return err
	}
	source, err := e.client.Message.Get(ctx, item.MessageID)
	if err != nil {
		return err
	}
	_, err = e.store.Attach(ctx, mailstore.Attach{
		MailboxID:    mailboxID,
		MessageID:    item.MessageID,
		FolderID:     inbox.ID,
		SizeBytes:    source.SizeBytes,
		OriginalRcpt: item.OriginalRcpt,
		BaseRcpt:     item.BaseRcpt,
		EnforceQuota: true,
	})
	if ent.IsConstraintError(err) || errors.Is(err, mailstore.ErrOverQuota) {
		return nil
	}
	return err
}

func conditionsMatch(conditions map[string]any, msg *ent.Message, route routing.Result) bool {
	if len(conditions) == 0 {
		return true
	}
	for key, value := range conditions {
		expected := strings.ToLower(stringValue(value))
		switch key {
		case "plus_tag":
			if strings.ToLower(route.PlusTag) != expected {
				return false
			}
		case "original_rcpt":
			if strings.ToLower(route.OriginalRecipient) != expected {
				return false
			}
		case "base_rcpt":
			if strings.ToLower(route.BaseRecipient) != expected {
				return false
			}
		case "from":
			if !containsString(msg.FromAddresses, expected) {
				return false
			}
		case "subject_contains":
			if !strings.Contains(strings.ToLower(msg.Subject), expected) {
				return false
			}
		}
	}
	return true
}

func actionString(action map[string]any, key string) string {
	return strings.TrimSpace(stringValue(action[key]))
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(value, expected) {
			return true
		}
	}
	return false
}

func firstDomain(addresses []string) string {
	if len(addresses) == 0 {
		return ""
	}
	_, domain, _ := strings.Cut(strings.ToLower(addresses[0]), "@")
	return domain
}

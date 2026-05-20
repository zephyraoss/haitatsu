package rules

import (
	"context"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/routingrule"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

type Engine struct {
	client *ent.Client
}

type Delivery struct {
	Message *ent.MailboxMessage
	Route   routing.Result
}

func New(client *ent.Client) *Engine {
	return &Engine{client: client}
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
			if err := e.applyAction(ctx, delivery.Message, action); err != nil {
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

func (e *Engine) applyAction(ctx context.Context, item *ent.MailboxMessage, action map[string]any) error {
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
		_, err := e.client.MailboxMessage.UpdateOneID(item.ID).SetDeletedAt(time.Now()).Save(ctx)
		return err
	case "copy_to_mailbox":
		return e.copyToMailbox(ctx, item, actionString(action, "mailbox_id"))
	default:
		return nil
	}
}

func (e *Engine) move(ctx context.Context, item *ent.MailboxMessage, action map[string]any) error {
	if folderID := actionString(action, "folder_id"); folderID != "" {
		_, err := e.client.MailboxMessage.UpdateOneID(item.ID).SetFolderID(folderID).Save(ctx)
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
	folder, err := e.client.Folder.Query().Where(folder.MailboxIDEQ(item.MailboxID), folder.NameEQ(folderName)).Only(ctx)
	if err != nil {
		return err
	}
	_, err = e.client.MailboxMessage.UpdateOneID(item.ID).SetFolderID(folder.ID).Save(ctx)
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
	_, err := e.client.MailboxMessageLabel.Create().SetMailboxMessageID(item.ID).SetLabelID(labelID).Save(ctx)
	if ent.IsConstraintError(err) {
		return nil
	}
	return err
}

func (e *Engine) copyToMailbox(ctx context.Context, item *ent.MailboxMessage, mailboxID string) error {
	if mailboxID == "" || mailboxID == item.MailboxID {
		return nil
	}
	inbox, err := e.client.Folder.Query().Where(folder.MailboxIDEQ(mailboxID), folder.NameEQ("INBOX")).Only(ctx)
	if err != nil {
		return err
	}
	_, err = e.client.MailboxMessage.Create().
		SetMailboxID(mailboxID).
		SetMessageID(item.MessageID).
		SetFolderID(inbox.ID).
		SetOriginalRcpt(item.OriginalRcpt).
		SetBaseRcpt(item.BaseRcpt).
		Save(ctx)
	if ent.IsConstraintError(err) {
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

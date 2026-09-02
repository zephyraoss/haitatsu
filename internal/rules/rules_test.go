package rules

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type captureSink struct {
	events []string
}

func (c *captureSink) Emit(_ context.Context, eventType string, _ string, _ map[string]any) error {
	c.events = append(c.events, eventType)
	return nil
}

func TestRulesMoveLabelAndWebhook(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	store := testutil.NewMailStore(t, client)
	sink := &captureSink{}
	engine := New(client, store, sink)
	mbox := testutil.SeedMailbox(t, store, "rules@example.test")
	inbox, _ := store.FolderByName(ctx, mbox.ID, "INBOX")
	archive, _ := store.FolderByName(ctx, mbox.ID, "Archive")
	if _, err := store.CreateLabel(ctx, mbox.ID, "Receipts"); err != nil {
		t.Fatal(err)
	}
	msg, err := client.Message.Create().SetID(ids.New().String()).SetTraceID("t").SetBlobKey("k").SetSha256("s").SetSizeBytes(5).SetFromAddresses([]string{"shop@vendor.test"}).SetSubject("Your receipt").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Attach(ctx, mailstore.Attach{MailboxID: mbox.ID, MessageID: msg.ID, FolderID: inbox.ID, SizeBytes: 5, PlusTag: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RoutingRule.Create().SetScope("mailbox").SetScopeRef(mbox.ID).SetName("receipts").SetConditions(map[string]any{"subject_contains": "receipt", "plus_tag": "shop"}).SetActions([]map[string]any{
		{"type": "move", "folder": "Archive"},
		{"type": "add_label", "label": "Receipts"},
		{"type": "mark_read"},
		{"type": "webhook", "event_type": "receipt.filed"},
	}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RoutingRule.Create().SetScope("global").SetName("never").SetConditions(map[string]any{"from": "nobody@nowhere.test"}).SetActions([]map[string]any{{"type": "delete"}}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	route := routing.Result{OriginalRecipient: "rules+shop@example.test", BaseRecipient: "rules@example.test", PlusTag: "shop"}
	if err := engine.Apply(ctx, msg, []Delivery{{Message: item, Route: route}}); err != nil {
		t.Fatal(err)
	}
	after, _ := client.MailboxMessage.Get(ctx, item.ID)
	if after.FolderID != archive.ID || !after.Read || after.DeletedAt != nil {
		t.Fatalf("rule actions not applied: %+v", after)
	}
	links, _ := client.MailboxMessageLabel.Query().All(ctx)
	if len(links) != 1 || links[0].UID != 1 {
		t.Fatalf("label link = %+v", links)
	}
	if len(sink.events) != 1 || sink.events[0] != "receipt.filed" {
		t.Fatalf("events = %v", sink.events)
	}
	_ = ent.IsNotFound
}

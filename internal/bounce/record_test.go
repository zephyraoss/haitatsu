package bounce

import (
	"context"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/testutil"
)

type recordedEvent struct {
	eventType string
	payload   map[string]any
}

type recordingEvents struct{ events []recordedEvent }

func (r *recordingEvents) Emit(_ context.Context, eventType string, _ string, payload map[string]any) error {
	r.events = append(r.events, recordedEvent{eventType: eventType, payload: payload})
	return nil
}

func TestRecordEmitsEventOnlyForDomainSends(t *testing.T) {
	ctx := context.Background()
	client, _ := testutil.NewClient(t)
	emitter := &recordingEvents{}
	handler := NewHandler(client, testutil.NewFakeStore(), metrics.New()).WithEvents(emitter)
	if _, err := client.OutboundJob.Create().SetMailboxID("").SetSenderDomain("example.test").SetMessageID("domain-msg").SetReturnPath("bounces+domain-msg@example.test").SetRecipients([]string{"a@x.test"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.OutboundJob.Create().SetMailboxID("m").SetMessageID("mailbox-msg").SetReturnPath("bounces+mailbox-msg@example.test").SetRecipients([]string{"b@x.test"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	recipients := []Recipient{
		{Address: "bounces+domain-msg@example.test", MessageID: "domain-msg"},
		{Address: "bounces+mailbox-msg@example.test", MessageID: "mailbox-msg"},
	}
	if err := handler.Record(ctx, recipients, dsnMessage(1, 0)); err != nil {
		t.Fatal(err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("events = %+v", emitter.events)
	}
	event := emitter.events[0]
	if event.eventType != events.MessageBounced || event.payload["source"] != "dsn" || event.payload["message_id"] != "domain-msg" || event.payload["sender_domain"] != "example.test" || event.payload["bounce_event_id"] == "" {
		t.Fatalf("unexpected event %+v", event)
	}
}

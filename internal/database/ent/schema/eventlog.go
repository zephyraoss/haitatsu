package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type EventLog struct {
	ent.Schema
}

func (EventLog) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("event_type"),
		field.String("trace_id").Optional(),
		field.String("message_id").Optional(),
		field.String("mailbox_id").Optional(),
		field.JSON("payload", map[string]any{}),
		field.String("status").Default("queued"),
		field.Int("attempts").Default(0),
		field.String("locked_by").Optional(),
		field.Time("locked_until").Optional().Nillable(),
		field.Time("next_attempt_at").Optional().Nillable(),
		field.JSON("last_error", map[string]any{}).Optional(),
		createdAtField(),
		updatedAtField(),
	}
}

func (EventLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("event_type"),
		index.Fields("message_id"),
		index.Fields("mailbox_id"),
		index.Fields("trace_id"),
		index.Fields("status", "next_attempt_at"),
		index.Fields("locked_until"),
		index.Fields("created_at"),
	}
}

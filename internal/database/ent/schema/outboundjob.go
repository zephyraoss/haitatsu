package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type OutboundJob struct {
	ent.Schema
}

func (OutboundJob) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("message_id"),
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

func (OutboundJob) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "next_attempt_at"),
		index.Fields("locked_until"),
		index.Fields("message_id"),
	}
}

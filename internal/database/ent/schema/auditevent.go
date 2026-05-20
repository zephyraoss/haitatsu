package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type AuditEvent struct {
	ent.Schema
}

func (AuditEvent) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("event_type"),
		field.String("actor_type"),
		field.String("actor_id").Optional(),
		field.String("entity_type"),
		field.String("entity_id"),
		field.String("mailbox_id").Optional(),
		field.String("trace_id").Optional(),
		field.JSON("details", map[string]any{}).Optional(),
		createdAtField(),
	}
}

func (AuditEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("event_type"),
		index.Fields("mailbox_id"),
		index.Fields("trace_id"),
		index.Fields("created_at"),
	}
}

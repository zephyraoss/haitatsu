package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type ImportJob struct {
	ent.Schema
}

func (ImportJob) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("source_type"),
		field.JSON("source", map[string]any{}),
		field.String("status").Default("queued"),
		field.Int("imported_count").Default(0),
		field.String("locked_by").Optional(),
		field.Time("locked_until").Optional().Nillable(),
		field.JSON("last_error", map[string]any{}).Optional(),
		createdAtField(),
		updatedAtField(),
	}
}

func (ImportJob) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_id"),
		index.Fields("source_type"),
		index.Fields("status"),
		index.Fields("locked_until"),
	}
}

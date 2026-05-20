package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type ExportJob struct {
	ent.Schema
}

func (ExportJob) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("status").Default("queued"),
		field.String("object_key").Optional(),
		field.Int64("size_bytes").Default(0),
		field.String("locked_by").Optional(),
		field.Time("locked_until").Optional().Nillable(),
		field.JSON("last_error", map[string]any{}).Optional(),
		field.Time("expires_at").Optional().Nillable(),
		createdAtField(),
		updatedAtField(),
	}
}

func (ExportJob) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_id"),
		index.Fields("status"),
		index.Fields("locked_until"),
		index.Fields("expires_at"),
	}
}

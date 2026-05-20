package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type BounceEvent struct {
	ent.Schema
}

func (BounceEvent) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("message_id"),
		field.String("recipient"),
		field.String("blob_key"),
		field.String("sha256"),
		field.Int64("size_bytes"),
		field.JSON("details", map[string]any{}).Optional(),
		createdAtField(),
	}
}

func (BounceEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("message_id"),
		index.Fields("created_at"),
	}
}

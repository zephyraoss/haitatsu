package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Label struct {
	ent.Schema
}

func (Label) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("name"),
		createdAtField(),
		updatedAtField(),
	}
}

func (Label) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_id", "name").Unique(),
	}
}

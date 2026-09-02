package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Folder struct {
	ent.Schema
}

func (Folder) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("name"),
		field.Bool("system").Default(false),
		field.Uint32("uid_validity").Default(1),
		field.Uint32("uid_next").Default(1),
		createdAtField(),
		updatedAtField(),
	}
}

func (Folder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_id", "name").Unique(),
	}
}

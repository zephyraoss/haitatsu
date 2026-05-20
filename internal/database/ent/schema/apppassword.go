package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type AppPassword struct {
	ent.Schema
}

func (AppPassword) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("name"),
		field.String("hash"),
		field.JSON("scopes", []string{}),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
		createdAtField(),
		updatedAtField(),
		deletedAtField(),
	}
}

func (AppPassword) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_id"),
		index.Fields("revoked_at"),
		index.Fields("deleted_at"),
	}
}

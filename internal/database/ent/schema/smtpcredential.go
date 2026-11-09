package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type SMTPCredential struct {
	ent.Schema
}

func (SMTPCredential) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("username").Unique().Immutable(),
		field.String("name"),
		field.String("hash").Sensitive(),
		field.JSON("domains", []string{}),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
		createdAtField(),
		updatedAtField(),
	}
}

func (SMTPCredential) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("revoked_at"),
	}
}

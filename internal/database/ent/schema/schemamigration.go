package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

type SchemaMigration struct {
	ent.Schema
}

func (SchemaMigration) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.Time("applied_at"),
	}
}

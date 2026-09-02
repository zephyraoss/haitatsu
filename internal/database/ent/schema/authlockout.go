package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type AuthLockout struct {
	ent.Schema
}

func (AuthLockout) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.Int("failures").Default(0),
		field.Time("window_start"),
		field.Time("locked_until").Optional().Nillable(),
		updatedAtField(),
	}
}

func (AuthLockout) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("locked_until"),
		index.Fields("window_start"),
	}
}

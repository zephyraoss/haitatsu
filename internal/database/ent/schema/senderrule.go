package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type SenderRule struct {
	ent.Schema
}

func (SenderRule) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("scope"),
		field.String("scope_ref").Optional(),
		field.String("kind"),
		field.String("match_type"),
		field.String("value"),
		field.String("action").Default("junk"),
		createdAtField(),
		updatedAtField(),
	}
}

func (SenderRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope", "scope_ref", "kind"),
		index.Fields("match_type", "value"),
	}
}

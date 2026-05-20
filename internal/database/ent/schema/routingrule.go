package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type RoutingRule struct {
	ent.Schema
}

func (RoutingRule) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("scope"),
		field.String("scope_ref").Optional(),
		field.Int("priority").Default(0),
		field.String("name"),
		field.Bool("enabled").Default(true),
		field.JSON("conditions", map[string]any{}),
		field.JSON("actions", []map[string]any{}),
		createdAtField(),
		updatedAtField(),
	}
}

func (RoutingRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope", "scope_ref", "priority"),
		index.Fields("enabled"),
	}
}

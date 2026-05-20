package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Route struct {
	ent.Schema
}

func (Route) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("source_address").Unique(),
		field.String("type"),
		field.JSON("destinations", []string{}),
		field.String("status").Default("active"),
		createdAtField(),
		updatedAtField(),
		deletedAtField(),
	}
}

func (Route) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("type"),
		index.Fields("status"),
		index.Fields("deleted_at"),
	}
}

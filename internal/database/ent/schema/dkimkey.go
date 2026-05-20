package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type DKIMKey struct {
	ent.Schema
}

func (DKIMKey) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("domain"),
		field.String("selector").Default("zpr1"),
		field.String("private_key_pem"),
		field.String("public_key_pem"),
		createdAtField(),
		updatedAtField(),
	}
}

func (DKIMKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("domain", "selector").Unique(),
	}
}

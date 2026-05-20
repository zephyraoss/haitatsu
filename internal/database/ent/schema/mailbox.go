package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Mailbox struct {
	ent.Schema
}

func (Mailbox) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("primary_address").Unique(),
		field.String("status").Default("active"),
		field.Int64("quota_bytes").Default(0),
		field.Int64("used_bytes").Default(0),
		field.JSON("outbound_limits", map[string]int64{}).Optional(),
		createdAtField(),
		updatedAtField(),
		deletedAtField(),
	}
}

func (Mailbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("deleted_at"),
	}
}

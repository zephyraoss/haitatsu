package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type MailboxMessageLabel struct {
	ent.Schema
}

func (MailboxMessageLabel) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_message_id"),
		field.String("label_id"),
		createdAtField(),
	}
}

func (MailboxMessageLabel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_message_id", "label_id").Unique(),
		index.Fields("label_id"),
	}
}

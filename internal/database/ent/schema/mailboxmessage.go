package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type MailboxMessage struct {
	ent.Schema
}

func (MailboxMessage) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("mailbox_id"),
		field.String("message_id"),
		field.String("folder_id"),
		field.Uint32("uid").Default(0),
		field.String("original_rcpt"),
		field.String("base_rcpt"),
		field.String("plus_tag").Optional(),
		field.String("resolved_route_id").Optional(),
		field.Bool("read").Default(false),
		field.Bool("flagged").Default(false),
		field.Bool("answered").Default(false),
		field.Bool("draft").Default(false),
		field.Bool("imap_deleted").Default(false),
		field.JSON("keywords", []string{}).Optional(),
		createdAtField(),
		updatedAtField(),
		deletedAtField(),
	}
}

func (MailboxMessage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("mailbox_id", "message_id").Unique(),
		index.Fields("mailbox_id", "folder_id"),
		index.Fields("folder_id", "uid"),
		index.Fields("message_id"),
		index.Fields("deleted_at"),
	}
}

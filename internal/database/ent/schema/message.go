package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Message struct {
	ent.Schema
}

func (Message) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("trace_id"),
		field.String("rfc_message_id").Optional(),
		field.String("blob_key"),
		field.String("sha256"),
		field.Int64("size_bytes"),
		field.JSON("headers", map[string][]string{}).Optional(),
		field.JSON("from_addresses", []string{}).Optional(),
		field.JSON("to_addresses", []string{}).Optional(),
		field.JSON("cc_addresses", []string{}).Optional(),
		field.JSON("bcc_addresses", []string{}).Optional(),
		field.String("subject").Optional(),
		field.Time("date").Optional().Nillable(),
		field.String("text_body_extract").Optional(),
		field.String("html_body_extract").Optional(),
		field.JSON("attachments", []map[string]any{}).Optional(),
		field.Float("spam_score").Default(0),
		field.JSON("auth_results", map[string]any{}).Optional(),
		createdAtField(),
		updatedAtField(),
		deletedAtField(),
	}
}

func (Message) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("trace_id"),
		index.Fields("rfc_message_id"),
		index.Fields("created_at"),
		index.Fields("deleted_at"),
	}
}

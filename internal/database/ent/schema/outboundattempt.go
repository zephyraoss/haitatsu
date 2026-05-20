package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type OutboundAttempt struct {
	ent.Schema
}

func (OutboundAttempt) Fields() []ent.Field {
	return []ent.Field{
		ulidField(),
		field.String("outbound_job_id"),
		field.String("message_id"),
		field.Int("smtp_code").Optional(),
		field.String("enhanced_status_code").Optional(),
		field.String("classification"),
		field.String("response").Optional(),
		createdAtField(),
	}
}

func (OutboundAttempt) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("outbound_job_id"),
		index.Fields("message_id"),
		index.Fields("classification"),
	}
}

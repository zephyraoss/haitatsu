package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"

	"github.com/zephyraoss/haitatsu/internal/ids"
)

func ulidField() ent.Field {
	return field.String("id").
		DefaultFunc(func() string { return ids.New().String() }).
		Immutable().
		Unique()
}

func createdAtField() ent.Field {
	return field.Time("created_at").Default(time.Now).Immutable()
}

func updatedAtField() ent.Field {
	return field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now)
}

func deletedAtField() ent.Field {
	return field.Time("deleted_at").Optional().Nillable()
}

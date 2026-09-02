package api

import (
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v3"
)

type cursor struct {
	CreatedAt string
	ID        string
}

func decodeCursor(value string) (cursor, bool) {
	if value == "" {
		return cursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, false
	}
	createdAt, id, ok := strings.Cut(string(raw), "|")
	if !ok || createdAt == "" || id == "" {
		return cursor{}, false
	}
	return cursor{CreatedAt: createdAt, ID: id}, true
}

func encodeCursor(createdAt string, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + id))
}

func requestCursor(c fiber.Ctx) (cursor, bool, bool) {
	value := c.Query("cursor")
	if value == "" {
		return cursor{}, false, true
	}
	parsed, ok := decodeCursor(value)
	return parsed, ok, ok
}

func nextCursor[T any](items []T, limit int, key func(T) (string, string)) ([]T, string) {
	if len(items) <= limit {
		return items, ""
	}
	last := items[limit-1]
	createdAt, id := key(last)
	return items[:limit], encodeCursor(createdAt, id)
}

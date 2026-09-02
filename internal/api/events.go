package api

import (
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/eventlog"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
)

func (h *Handler) listEvents(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.EventLog.Query().Order(eventlog.ByCreatedAt(entsql.OrderDesc()), eventlog.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.EventLog](eventlog.FieldCreatedAt, eventlog.FieldID, cur))
	}
	if mailboxID := c.Query("mailbox_id"); mailboxID != "" {
		query.Where(eventlog.MailboxIDEQ(mailboxID))
	}
	if messageID := c.Query("message_id"); messageID != "" {
		query.Where(eventlog.MessageIDEQ(messageID))
	}
	if eventType := c.Query("event_type"); eventType != "" {
		query.Where(eventlog.EventTypeEQ(eventType))
	}
	if traceID := c.Query("trace_id"); traceID != "" {
		query.Where(eventlog.TraceIDEQ(traceID))
	}
	if after := c.Query("after"); after != "" {
		parsed, err := time.Parse(time.RFC3339, after)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_after", "after must be RFC3339")
		}
		query.Where(eventlog.CreatedAtGTE(parsed))
	}
	if before := c.Query("before"); before != "" {
		parsed, err := time.Parse(time.RFC3339, before)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_before", "before must be RFC3339")
		}
		query.Where(eventlog.CreatedAtLTE(parsed))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "event_list_failed", "Failed to list events")
	}
	page, next := nextCursor(items, limit, func(item *ent.EventLog) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

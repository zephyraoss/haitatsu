package api

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent/eventlog"
)

func (h *Handler) listEvents(c fiber.Ctx) error {
	limit := requestLimit(c)
	query := h.client.EventLog.Query().Limit(limit)
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
	return list(c, items, limit, "")
}

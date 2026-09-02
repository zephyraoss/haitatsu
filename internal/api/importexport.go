package api

import (
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/exportjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/importjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
	"github.com/zephyraoss/haitatsu/internal/importexport"
)

func redactedImportJob(job *ent.ImportJob) *ent.ImportJob {
	if job == nil || job.Source == nil {
		return job
	}
	copied := *job
	copied.Source = importexport.RedactSource(job.Source)
	return &copied
}

type importRequest struct {
	SourceType string         `json:"source_type"`
	Source     map[string]any `json:"source"`
}

func (h *Handler) createExport(c fiber.Ctx) error {
	job, err := h.client.ExportJob.Create().SetMailboxID(c.Params("mailbox_id")).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "export_create_failed", "Failed to create export")
	}
	return created(c, job)
}

func (h *Handler) listExports(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.ExportJob.Query().Order(exportjob.ByCreatedAt(entsql.OrderDesc()), exportjob.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.ExportJob](exportjob.FieldCreatedAt, exportjob.FieldID, cur))
	}
	if mailboxID := c.Query("mailbox_id"); mailboxID != "" {
		query.Where(exportjob.MailboxIDEQ(mailboxID))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "export_list_failed", "Failed to list exports")
	}
	page, next := nextCursor(items, limit, func(item *ent.ExportJob) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

func (h *Handler) getExport(c fiber.Ctx) error {
	job, err := h.client.ExportJob.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "export_not_found", "Export not found")
	}
	return data(c, job)
}

func (h *Handler) createImport(c fiber.Ctx) error {
	var req importRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.SourceType == "" {
		return problem(c, fiber.StatusBadRequest, "source_type_required", "Source type is required")
	}
	if req.Source == nil {
		req.Source = map[string]any{}
	}
	switch req.SourceType {
	case "zip":
		if key, _ := req.Source["object_key"].(string); key == "" {
			if key, _ := req.Source["key"].(string); key == "" {
				return problem(c, fiber.StatusBadRequest, "source_invalid", "Zip import requires source.object_key")
			}
		}
	case "maildir":
		if path, _ := req.Source["path"].(string); path == "" {
			return problem(c, fiber.StatusBadRequest, "source_invalid", "Maildir import requires source.path")
		}
	case "imap":
		if addr, _ := req.Source["addr"].(string); addr == "" {
			return problem(c, fiber.StatusBadRequest, "source_invalid", "IMAP import requires source.addr")
		}
	default:
		return problem(c, fiber.StatusBadRequest, "source_type_invalid", "Source type must be one of: zip, maildir, imap")
	}
	job, err := h.client.ImportJob.Create().SetMailboxID(c.Params("mailbox_id")).SetSourceType(req.SourceType).SetSource(req.Source).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "import_create_failed", "Failed to create import")
	}
	return created(c, redactedImportJob(job))
}

func (h *Handler) listImports(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.ImportJob.Query().Order(importjob.ByCreatedAt(entsql.OrderDesc()), importjob.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.ImportJob](importjob.FieldCreatedAt, importjob.FieldID, cur))
	}
	if mailboxID := c.Query("mailbox_id"); mailboxID != "" {
		query.Where(importjob.MailboxIDEQ(mailboxID))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "import_list_failed", "Failed to list imports")
	}
	page, next := nextCursor(items, limit, func(item *ent.ImportJob) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	redacted := make([]*ent.ImportJob, len(page))
	for i, item := range page {
		redacted[i] = redactedImportJob(item)
	}
	return list(c, redacted, limit, next)
}

func (h *Handler) getImport(c fiber.Ctx) error {
	job, err := h.client.ImportJob.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "import_not_found", "Import not found")
	}
	return data(c, redactedImportJob(job))
}

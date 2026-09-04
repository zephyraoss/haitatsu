package api

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/apppassword"
	"github.com/zephyraoss/haitatsu/internal/database/ent/auditevent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
	"github.com/zephyraoss/haitatsu/internal/database/ent/route"
	"github.com/zephyraoss/haitatsu/internal/database/ent/routingrule"
	"github.com/zephyraoss/haitatsu/internal/mailaddr"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/outbound"
)

type Handler struct {
	client   *ent.Client
	db       *sql.DB
	dialect  string
	store    MessageStore
	mail     *mailstore.Store
	outbound *outbound.Submission
	config   *config.Holder
	reloader Reloader
}

type Reloader interface {
	Reload(ctx context.Context) error
}

type MessageStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
}

func Register(router fiber.Router, client *ent.Client, db *sql.DB, store MessageStore, mail *mailstore.Store, outboundService *outbound.Submission, cfg *config.Holder, reloader Reloader) {
	dbDialect := dialect.Postgres
	if cfg.Get().EffectiveDatabase().Driver != "postgres" {
		dbDialect = dialect.SQLite
	}
	h := &Handler{client: client, db: db, dialect: dbDialect, store: store, mail: mail, outbound: outboundService, config: cfg, reloader: reloader}
	v1 := router.Group("/api/v1", ServiceTokenMiddleware(func() string { return cfg.Get().API.ServiceToken }))

	v1.Get("/mailboxes", h.listMailboxes)
	v1.Post("/mailboxes", h.createMailbox)
	v1.Get("/mailboxes/:id", h.getMailbox)
	v1.Post("/mailboxes/:id/recompute-usage", h.recomputeMailboxUsage)
	v1.Patch("/mailboxes/:id", h.updateMailbox)
	v1.Delete("/mailboxes/:id", h.deleteMailbox)
	v1.Post("/mailboxes/:id/restore", h.restoreMailbox)
	v1.Delete("/mailboxes/:id/hard", h.hardDeleteMailbox)

	v1.Get("/mailboxes/:mailbox_id/passwords", h.listAppPasswords)
	v1.Post("/mailboxes/:mailbox_id/passwords", h.createAppPassword)
	v1.Delete("/mailboxes/:mailbox_id/passwords/:id", h.revokeAppPassword)

	v1.Get("/routes", h.listRoutes)
	v1.Post("/routes", h.createRoute)
	v1.Get("/routes/:id", h.getRoute)
	v1.Patch("/routes/:id", h.updateRoute)
	v1.Delete("/routes/:id", h.deleteRoute)

	v1.Get("/routing/rules", h.listRoutingRules)
	v1.Post("/routing/rules", h.createRoutingRule)
	v1.Patch("/routing/rules/:id", h.updateRoutingRule)
	v1.Delete("/routing/rules/:id", h.deleteRoutingRule)

	v1.Get("/mailboxes/:mailbox_id/folders", h.listFolders)
	v1.Post("/mailboxes/:mailbox_id/folders", h.createFolder)
	v1.Patch("/folders/:id", h.updateFolder)
	v1.Delete("/folders/:id", h.deleteFolder)

	v1.Get("/mailboxes/:mailbox_id/labels", h.listLabels)
	v1.Post("/mailboxes/:mailbox_id/labels", h.createLabel)
	v1.Patch("/labels/:id", h.updateLabel)
	v1.Delete("/labels/:id", h.deleteLabel)

	v1.Get("/mailboxes/:mailbox_id/messages", h.listMessages)
	v1.Get("/messages/:id", h.getMessage)
	v1.Get("/messages/:id/raw", h.downloadRawMessage)
	v1.Get("/messages/:id/attachments/:attachment_id", h.downloadAttachment)
	v1.Post("/mailboxes/:mailbox_id/outbound", h.createOutboundMessage)
	v1.Patch("/messages/:id", h.updateMailboxMessage)
	v1.Post("/messages/:id/move", h.moveMessage)
	v1.Delete("/messages/:id", h.deleteMessage)
	v1.Post("/messages/:id/restore", h.restoreMessage)
	v1.Post("/messages/:id/labels", h.addMessageLabel)
	v1.Delete("/messages/:id/labels/:label_id", h.removeMessageLabel)

	v1.Get("/audit", h.listAuditEvents)
	v1.Get("/events", h.listEvents)
	v1.Post("/admin/reload", h.reloadConfig)
	v1.Get("/dns/check/:domain", h.checkDNS)

	v1.Get("/dkim", h.listDKIMKeys)
	v1.Post("/dkim", h.createDKIMKey)
	v1.Get("/dkim/:id", h.getDKIMKey)

	v1.Get("/rules/sender", h.listSenderRules)
	v1.Post("/rules/sender", h.createSenderRule)
	v1.Delete("/rules/sender/:id", h.deleteSenderRule)

	v1.Post("/mailboxes/:mailbox_id/export", h.createExport)
	v1.Get("/exports", h.listExports)
	v1.Get("/exports/:id", h.getExport)
	v1.Post("/mailboxes/:mailbox_id/import", h.createImport)
	v1.Get("/imports", h.listImports)
	v1.Get("/imports/:id", h.getImport)
}

type mailboxRequest struct {
	PrimaryAddress string           `json:"primary_address"`
	Status         string           `json:"status"`
	QuotaBytes     *int64           `json:"quota_bytes"`
	OutboundLimits map[string]int64 `json:"outbound_limits"`
}

type routeRequest struct {
	SourceAddress string   `json:"source_address"`
	Type          string   `json:"type"`
	Destinations  []string `json:"destinations"`
	Status        string   `json:"status"`
}

type appPasswordRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type folderRequest struct {
	Name   string `json:"name"`
	System *bool  `json:"system"`
}

type labelRequest struct {
	Name string `json:"name"`
}

type routingRuleRequest struct {
	Scope      string           `json:"scope"`
	ScopeRef   string           `json:"scope_ref"`
	Priority   *int             `json:"priority"`
	Name       string           `json:"name"`
	Enabled    *bool            `json:"enabled"`
	Conditions map[string]any   `json:"conditions"`
	Actions    []map[string]any `json:"actions"`
}

type appPasswordResponse struct {
	ID         string     `json:"id"`
	MailboxID  string     `json:"mailbox_id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
	Password   string     `json:"password,omitempty"`
}

func (h *Handler) listMailboxes(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.Mailbox.Query().Order(mailbox.ByCreatedAt(entsql.OrderDesc()), mailbox.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if status := c.Query("status"); status != "" {
		query.Where(mailbox.StatusEQ(status))
	}
	if hasCursor {
		query.Where(cursorPredicate[predicate.Mailbox](mailbox.FieldCreatedAt, mailbox.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "mailbox_list_failed", "Failed to list mailboxes")
	}
	page, next := nextCursor(items, limit, func(m *ent.Mailbox) (string, string) { return cursorTime(m.CreatedAt), m.ID })
	return list(c, page, limit, next)
}

func (h *Handler) recomputeMailboxUsage(c fiber.Ctx) error {
	total, err := h.mail.RecomputeUsedBytes(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "mailbox_recompute_failed", "Failed to recompute mailbox usage")
	}
	return data(c, fiber.Map{"mailbox_id": c.Params("id"), "used_bytes": total})
}

func (h *Handler) createMailbox(c fiber.Ctx) error {
	var req mailboxRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.PrimaryAddress == "" {
		return problem(c, fiber.StatusBadRequest, "primary_address_required", "Primary address is required")
	}
	if err := mailaddr.ValidateAddressNotReserved(req.PrimaryAddress); err != nil {
		return problem(c, fiber.StatusBadRequest, "reserved_local_part", "The bounces local part is reserved for system VERP")
	}

	create := h.client.Mailbox.Create().SetPrimaryAddress(req.PrimaryAddress)
	if req.QuotaBytes != nil {
		create.SetQuotaBytes(*req.QuotaBytes)
	}
	if req.OutboundLimits != nil {
		if err := validateOutboundLimits(req.OutboundLimits); err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_outbound_limits", err.Error())
		}
		create.SetOutboundLimits(req.OutboundLimits)
	}
	if req.Status != "" {
		create.SetStatus(req.Status)
	}

	mbox, err := create.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "mailbox_create_failed", "Failed to create mailbox")
	}
	if err := h.mail.CreateDefaultFolders(c.Context(), mbox.ID); err != nil {
		return problem(c, fiber.StatusInternalServerError, "default_folders_failed", "Failed to create default folders")
	}
	if err := h.audit(c, "mailbox.created", "mailbox", mbox.ID, mbox.ID, nil); err != nil {
		return err
	}
	return created(c, mbox)
}

func (h *Handler) getMailbox(c fiber.Ctx) error {
	mbox, err := h.client.Mailbox.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "mailbox_not_found", "Mailbox not found")
	}
	return data(c, mbox)
}

func (h *Handler) updateMailbox(c fiber.Ctx) error {
	var req mailboxRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}

	update := h.client.Mailbox.UpdateOneID(c.Params("id"))
	if req.PrimaryAddress != "" {
		if err := mailaddr.ValidateAddressNotReserved(req.PrimaryAddress); err != nil {
			return problem(c, fiber.StatusBadRequest, "reserved_local_part", "The bounces local part is reserved for system VERP")
		}
		update.SetPrimaryAddress(req.PrimaryAddress)
	}
	if req.Status != "" {
		update.SetStatus(req.Status)
	}
	if req.QuotaBytes != nil {
		update.SetQuotaBytes(*req.QuotaBytes)
	}
	if req.OutboundLimits != nil {
		if err := validateOutboundLimits(req.OutboundLimits); err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_outbound_limits", err.Error())
		}
		update.SetOutboundLimits(req.OutboundLimits)
	}

	mbox, err := update.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "mailbox_update_failed", "Failed to update mailbox")
	}
	if err := h.audit(c, "mailbox.updated", "mailbox", mbox.ID, mbox.ID, nil); err != nil {
		return err
	}
	return data(c, mbox)
}

func (h *Handler) deleteMailbox(c fiber.Ctx) error {
	now := time.Now()
	mbox, err := h.client.Mailbox.UpdateOneID(c.Params("id")).SetStatus("deleted").SetDeletedAt(now).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "mailbox_delete_failed", "Failed to delete mailbox")
	}
	_, err = h.client.AppPassword.Update().Where(apppassword.MailboxIDEQ(mbox.ID), apppassword.RevokedAtIsNil()).SetRevokedAt(now).SetDeletedAt(now).Save(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "app_password_disable_failed", "Failed to disable app passwords")
	}
	if err := h.audit(c, "mailbox.deleted", "mailbox", mbox.ID, mbox.ID, nil); err != nil {
		return err
	}
	return data(c, mbox)
}

func (h *Handler) restoreMailbox(c fiber.Ctx) error {
	mbox, err := h.client.Mailbox.UpdateOneID(c.Params("id")).SetStatus("active").ClearDeletedAt().Save(c.Context())
	if err != nil {
		return entProblem(c, err, "mailbox_restore_failed", "Failed to restore mailbox")
	}
	if err := h.audit(c, "mailbox.restored", "mailbox", mbox.ID, mbox.ID, nil); err != nil {
		return err
	}
	return data(c, mbox)
}

func (h *Handler) hardDeleteMailbox(c fiber.Ctx) error {
	id := c.Params("id")
	if _, err := h.client.Mailbox.Get(c.Context(), id); err != nil {
		return entProblem(c, err, "mailbox_not_found", "Mailbox not found")
	}
	if err := h.mail.PurgeMailbox(c.Context(), id); err != nil {
		return entProblem(c, err, "mailbox_hard_delete_failed", "Failed to hard delete mailbox")
	}
	if err := h.audit(c, "mailbox.hard_deleted", "mailbox", id, id, nil); err != nil {
		return err
	}
	return empty(c)
}

func (h *Handler) listAppPasswords(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.AppPassword.Query().Where(apppassword.MailboxIDEQ(c.Params("mailbox_id"))).Order(apppassword.ByCreatedAt(entsql.OrderDesc()), apppassword.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.AppPassword](apppassword.FieldCreatedAt, apppassword.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "app_password_list_failed", "Failed to list app passwords")
	}
	page, next := nextCursor(items, limit, func(item *ent.AppPassword) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, appPasswordList(page), limit, next)
}

func (h *Handler) createAppPassword(c fiber.Ctx) error {
	var req appPasswordRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.Name == "" || len(req.Scopes) == 0 {
		return problem(c, fiber.StatusBadRequest, "invalid_app_password", "Name and scopes are required")
	}
	if !validProtocolScopes(req.Scopes) {
		return problem(c, fiber.StatusBadRequest, "invalid_app_password_scope", "App password scopes must be protocol scopes")
	}

	password, err := passwordauth.GeneratePassword()
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "app_password_generate_failed", "Failed to generate app password")
	}
	hash, err := passwordauth.HashPassword(password)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "app_password_hash_failed", "Failed to hash app password")
	}

	item, err := h.client.AppPassword.Create().SetMailboxID(c.Params("mailbox_id")).SetName(req.Name).SetHash(hash).SetScopes(req.Scopes).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "app_password_create_failed", "Failed to create app password")
	}
	if err := h.audit(c, "app_password.created", "app_password", item.ID, item.MailboxID, nil); err != nil {
		return err
	}
	response := appPasswordPublic(item)
	response.Password = password
	return created(c, response)
}

func (h *Handler) revokeAppPassword(c fiber.Ctx) error {
	now := time.Now()
	item, err := h.client.AppPassword.UpdateOneID(c.Params("id")).SetRevokedAt(now).SetDeletedAt(now).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "app_password_revoke_failed", "Failed to revoke app password")
	}
	if err := h.audit(c, "app_password.revoked", "app_password", item.ID, item.MailboxID, nil); err != nil {
		return err
	}
	return data(c, appPasswordPublic(item))
}

func (h *Handler) listRoutes(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.Route.Query().Order(route.ByCreatedAt(entsql.OrderDesc()), route.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.Route](route.FieldCreatedAt, route.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "route_list_failed", "Failed to list routes")
	}
	page, next := nextCursor(items, limit, func(item *ent.Route) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

func (h *Handler) createRoute(c fiber.Ctx) error {
	var req routeRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.SourceAddress == "" || req.Type == "" {
		return problem(c, fiber.StatusBadRequest, "invalid_route", "Source address and type are required")
	}
	if err := mailaddr.ValidateAddressNotReserved(req.SourceAddress); err != nil {
		return problem(c, fiber.StatusBadRequest, "reserved_local_part", "The bounces local part is reserved for system VERP")
	}

	create := h.client.Route.Create().SetSourceAddress(req.SourceAddress).SetType(req.Type).SetDestinations(req.Destinations)
	if req.Status != "" {
		create.SetStatus(req.Status)
	}
	item, err := create.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "route_create_failed", "Failed to create route")
	}
	if err := h.audit(c, "route.created", "route", item.ID, "", nil); err != nil {
		return err
	}
	return created(c, item)
}

func (h *Handler) getRoute(c fiber.Ctx) error {
	item, err := h.client.Route.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "route_not_found", "Route not found")
	}
	return data(c, item)
}

func (h *Handler) updateRoute(c fiber.Ctx) error {
	var req routeRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	update := h.client.Route.UpdateOneID(c.Params("id"))
	if req.SourceAddress != "" {
		if err := mailaddr.ValidateAddressNotReserved(req.SourceAddress); err != nil {
			return problem(c, fiber.StatusBadRequest, "reserved_local_part", "The bounces local part is reserved for system VERP")
		}
		update.SetSourceAddress(req.SourceAddress)
	}
	if req.Type != "" {
		update.SetType(req.Type)
	}
	if req.Destinations != nil {
		update.SetDestinations(req.Destinations)
	}
	if req.Status != "" {
		update.SetStatus(req.Status)
	}
	item, err := update.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "route_update_failed", "Failed to update route")
	}
	if err := h.audit(c, "route.updated", "route", item.ID, "", nil); err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) deleteRoute(c fiber.Ctx) error {
	item, err := h.client.Route.UpdateOneID(c.Params("id")).SetStatus("deleted").SetDeletedAt(time.Now()).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "route_delete_failed", "Failed to delete route")
	}
	if err := h.audit(c, "route.deleted", "route", item.ID, "", nil); err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) listRoutingRules(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.RoutingRule.Query().Order(routingrule.ByCreatedAt(entsql.OrderDesc()), routingrule.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.RoutingRule](routingrule.FieldCreatedAt, routingrule.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "routing_rule_list_failed", "Failed to list routing rules")
	}
	page, next := nextCursor(items, limit, func(item *ent.RoutingRule) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

func (h *Handler) createRoutingRule(c fiber.Ctx) error {
	var req routingRuleRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.Scope == "" || req.Name == "" || len(req.Actions) == 0 {
		return problem(c, fiber.StatusBadRequest, "invalid_routing_rule", "Scope, name, and actions are required")
	}

	create := h.client.RoutingRule.Create().SetScope(req.Scope).SetName(req.Name).SetConditions(nonNilMap(req.Conditions)).SetActions(req.Actions)
	if req.ScopeRef != "" {
		create.SetScopeRef(req.ScopeRef)
	}
	if req.Priority != nil {
		create.SetPriority(*req.Priority)
	}
	if req.Enabled != nil {
		create.SetEnabled(*req.Enabled)
	}
	item, err := create.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "routing_rule_create_failed", "Failed to create routing rule")
	}
	if err := h.audit(c, "routing_rule.created", "routing_rule", item.ID, "", nil); err != nil {
		return err
	}
	return created(c, item)
}

func (h *Handler) updateRoutingRule(c fiber.Ctx) error {
	var req routingRuleRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	update := h.client.RoutingRule.UpdateOneID(c.Params("id"))
	if req.Scope != "" {
		update.SetScope(req.Scope)
	}
	if req.ScopeRef != "" {
		update.SetScopeRef(req.ScopeRef)
	}
	if req.Priority != nil {
		update.SetPriority(*req.Priority)
	}
	if req.Name != "" {
		update.SetName(req.Name)
	}
	if req.Enabled != nil {
		update.SetEnabled(*req.Enabled)
	}
	if req.Conditions != nil {
		update.SetConditions(req.Conditions)
	}
	if req.Actions != nil {
		update.SetActions(req.Actions)
	}
	item, err := update.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "routing_rule_update_failed", "Failed to update routing rule")
	}
	if err := h.audit(c, "routing_rule.updated", "routing_rule", item.ID, "", nil); err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) deleteRoutingRule(c fiber.Ctx) error {
	id := c.Params("id")
	if err := h.client.RoutingRule.DeleteOneID(id).Exec(c.Context()); err != nil {
		return entProblem(c, err, "routing_rule_delete_failed", "Failed to delete routing rule")
	}
	if err := h.audit(c, "routing_rule.deleted", "routing_rule", id, "", nil); err != nil {
		return err
	}
	return empty(c)
}

func (h *Handler) listFolders(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.Folder.Query().Where(folder.MailboxIDEQ(c.Params("mailbox_id"))).Order(folder.ByCreatedAt(entsql.OrderDesc()), folder.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.Folder](folder.FieldCreatedAt, folder.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "folder_list_failed", "Failed to list folders")
	}
	page, next := nextCursor(items, limit, func(item *ent.Folder) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

func (h *Handler) createFolder(c fiber.Ctx) error {
	var req folderRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.Name == "" {
		return problem(c, fiber.StatusBadRequest, "folder_name_required", "Folder name is required")
	}
	item, err := h.mail.CreateFolder(c.Context(), c.Params("mailbox_id"), req.Name, req.System != nil && *req.System)
	if err != nil {
		return entProblem(c, err, "folder_create_failed", "Failed to create folder")
	}
	if err := h.audit(c, "folder.created", "folder", item.ID, item.MailboxID, nil); err != nil {
		return err
	}
	return created(c, item)
}

func (h *Handler) updateFolder(c fiber.Ctx) error {
	var req folderRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	current, err := h.client.Folder.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "folder_not_found", "Folder not found")
	}

	update := h.client.Folder.UpdateOneID(current.ID)
	if req.Name != "" {
		update.SetName(req.Name)
	}
	if req.System != nil {
		update.SetSystem(*req.System)
	}
	item, err := update.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "folder_update_failed", "Failed to update folder")
	}
	if err := h.audit(c, "folder.updated", "folder", item.ID, item.MailboxID, nil); err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) deleteFolder(c fiber.Ctx) error {
	id := c.Params("id")
	item, err := h.client.Folder.Get(c.Context(), id)
	if err != nil {
		return entProblem(c, err, "folder_not_found", "Folder not found")
	}
	if item.System {
		return problem(c, fiber.StatusConflict, "system_folder", "System folders cannot be deleted")
	}
	active, err := h.mail.ActiveMessagesInFolder(c.Context(), item.ID)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "folder_delete_failed", "Failed to delete folder")
	}
	if err := h.mail.SoftDeleteMany(c.Context(), active); err != nil {
		return problem(c, fiber.StatusInternalServerError, "folder_delete_failed", "Failed to delete folder")
	}
	if err := h.client.Folder.DeleteOneID(id).Exec(c.Context()); err != nil {
		return entProblem(c, err, "folder_delete_failed", "Failed to delete folder")
	}
	if err := h.audit(c, "folder.deleted", "folder", id, item.MailboxID, nil); err != nil {
		return err
	}
	return empty(c)
}

func (h *Handler) listLabels(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.Label.Query().Where(label.MailboxIDEQ(c.Params("mailbox_id"))).Order(label.ByCreatedAt(entsql.OrderDesc()), label.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.Label](label.FieldCreatedAt, label.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "label_list_failed", "Failed to list labels")
	}
	page, next := nextCursor(items, limit, func(item *ent.Label) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

func (h *Handler) createLabel(c fiber.Ctx) error {
	var req labelRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.Name == "" {
		return problem(c, fiber.StatusBadRequest, "label_name_required", "Label name is required")
	}
	item, err := h.mail.CreateLabel(c.Context(), c.Params("mailbox_id"), req.Name)
	if err != nil {
		return entProblem(c, err, "label_create_failed", "Failed to create label")
	}
	if err := h.audit(c, "label.created", "label", item.ID, item.MailboxID, nil); err != nil {
		return err
	}
	return created(c, item)
}

func (h *Handler) updateLabel(c fiber.Ctx) error {
	var req labelRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.Name == "" {
		return problem(c, fiber.StatusBadRequest, "label_name_required", "Label name is required")
	}
	current, err := h.client.Label.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "label_not_found", "Label not found")
	}
	item, err := h.client.Label.UpdateOneID(current.ID).SetName(req.Name).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "label_update_failed", "Failed to update label")
	}
	if err := h.audit(c, "label.updated", "label", item.ID, item.MailboxID, nil); err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) deleteLabel(c fiber.Ctx) error {
	id := c.Params("id")
	item, err := h.client.Label.Get(c.Context(), id)
	if err != nil {
		return entProblem(c, err, "label_not_found", "Label not found")
	}
	if _, err := h.client.MailboxMessageLabel.Delete().Where(mailboxmessagelabel.LabelIDEQ(id)).Exec(c.Context()); err != nil {
		return problem(c, fiber.StatusInternalServerError, "label_delete_failed", "Failed to delete label")
	}
	if err := h.client.Label.DeleteOneID(id).Exec(c.Context()); err != nil {
		return entProblem(c, err, "label_delete_failed", "Failed to delete label")
	}
	if err := h.audit(c, "label.deleted", "label", id, item.MailboxID, nil); err != nil {
		return err
	}
	return empty(c)
}

func (h *Handler) listAuditEvents(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.AuditEvent.Query().Order(auditevent.ByCreatedAt(entsql.OrderDesc()), auditevent.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.AuditEvent](auditevent.FieldCreatedAt, auditevent.FieldID, cur))
	}
	if mailboxID := c.Query("mailbox_id"); mailboxID != "" {
		query.Where(auditevent.MailboxIDEQ(mailboxID))
	}
	if eventType := c.Query("event_type"); eventType != "" {
		query.Where(auditevent.EventTypeEQ(eventType))
	}
	if traceID := c.Query("trace_id"); traceID != "" {
		query.Where(auditevent.TraceIDEQ(traceID))
	}
	if after := c.Query("after"); after != "" {
		parsed, err := time.Parse(time.RFC3339, after)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_after", "after must be RFC3339")
		}
		query.Where(auditevent.CreatedAtGTE(parsed))
	}
	if before := c.Query("before"); before != "" {
		parsed, err := time.Parse(time.RFC3339, before)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_before", "before must be RFC3339")
		}
		query.Where(auditevent.CreatedAtLTE(parsed))
	}

	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "audit_event_list_failed", "Failed to list audit events")
	}
	page, next := nextCursor(items, limit, func(item *ent.AuditEvent) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	return list(c, page, limit, next)
}

func cursorTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func cursorPredicate[P ~func(*entsql.Selector)](createdAtField string, idField string, cur cursor) P {
	createdAt, err := time.Parse(time.RFC3339Nano, cur.CreatedAt)
	if err != nil {
		createdAt = time.Time{}
	}
	return P(func(s *entsql.Selector) {
		s.Where(entsql.Or(
			entsql.LT(s.C(createdAtField), createdAt),
			entsql.And(entsql.EQ(s.C(createdAtField), createdAt), entsql.LT(s.C(idField), cur.ID)),
		))
	})
}

func (h *Handler) audit(c fiber.Ctx, eventType string, entityType string, entityID string, mailboxID string, details map[string]any) error {
	create := h.client.AuditEvent.Create().
		SetEventType(eventType).
		SetActorType("service").
		SetActorID("service").
		SetEntityType(entityType).
		SetEntityID(entityID).
		SetDetails(nonNilMap(details))
	if mailboxID != "" {
		create.SetMailboxID(mailboxID)
	}
	_, err := create.Save(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "audit_event_failed", "Failed to write audit event")
	}
	return nil
}

func requestLimit(c fiber.Ctx) int {
	value, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil || value <= 0 {
		return 50
	}
	if value > 100 {
		return 100
	}
	return value
}

func entProblem(c fiber.Ctx, err error, code string, message string) error {
	if ent.IsNotFound(err) {
		return problem(c, fiber.StatusNotFound, code, message)
	}
	if ent.IsConstraintError(err) {
		return problem(c, fiber.StatusConflict, "constraint_violation", "Resource conflicts with existing data")
	}
	return problem(c, fiber.StatusInternalServerError, code, message)
}

func validateOutboundLimits(limits map[string]int64) error {
	for key, value := range limits {
		switch key {
		case "per_hour", "per_day", "recipients_per_message":
		default:
			return fmt.Errorf("unknown outbound limit %q (expected per_hour, per_day, recipients_per_message)", key)
		}
		if value < 0 {
			return fmt.Errorf("outbound limit %q must be >= 0", key)
		}
	}
	return nil
}

func validProtocolScopes(scopes []string) bool {
	for _, scope := range scopes {
		switch scope {
		case "imap", "smtp", "pop3":
		default:
			return false
		}
	}
	return true
}

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func appPasswordList(items []*ent.AppPassword) []appPasswordResponse {
	public := make([]appPasswordResponse, 0, len(items))
	for _, item := range items {
		public = append(public, appPasswordPublic(item))
	}
	return public
}

func appPasswordPublic(item *ent.AppPassword) appPasswordResponse {
	return appPasswordResponse{
		ID:         item.ID,
		MailboxID:  item.MailboxID,
		Name:       item.Name,
		Scopes:     item.Scopes,
		LastUsedAt: item.LastUsedAt,
		RevokedAt:  item.RevokedAt,
		CreatedAt:  item.CreatedAt,
		UpdatedAt:  item.UpdatedAt,
		DeletedAt:  item.DeletedAt,
	}
}

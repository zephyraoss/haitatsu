package api

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent/senderrule"
)

type senderRuleRequest struct {
	Scope     string `json:"scope"`
	ScopeRef  string `json:"scope_ref"`
	Kind      string `json:"kind"`
	MatchType string `json:"match_type"`
	Value     string `json:"value"`
	Action    string `json:"action"`
}

func (h *Handler) listSenderRules(c fiber.Ctx) error {
	limit := requestLimit(c)
	query := h.client.SenderRule.Query().Limit(limit)
	if scope := c.Query("scope"); scope != "" {
		query.Where(senderrule.ScopeEQ(scope))
	}
	if kind := c.Query("kind"); kind != "" {
		query.Where(senderrule.KindEQ(kind))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "sender_rule_list_failed", "Failed to list sender rules")
	}
	return list(c, items, limit, "")
}

func (h *Handler) createSenderRule(c fiber.Ctx) error {
	var req senderRuleRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if err := req.validate(); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_sender_rule", err.Error())
	}
	create := h.client.SenderRule.Create().
		SetScope(req.Scope).
		SetKind(req.Kind).
		SetMatchType(req.MatchType).
		SetValue(strings.ToLower(req.Value))
	if req.ScopeRef != "" {
		create.SetScopeRef(req.ScopeRef)
	}
	if req.Action != "" {
		create.SetAction(req.Action)
	}
	item, err := create.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "sender_rule_create_failed", "Failed to create sender rule")
	}
	return created(c, item)
}

func (h *Handler) deleteSenderRule(c fiber.Ctx) error {
	if err := h.client.SenderRule.DeleteOneID(c.Params("id")).Exec(c.Context()); err != nil {
		return entProblem(c, err, "sender_rule_delete_failed", "Failed to delete sender rule")
	}
	return empty(c)
}

func (r senderRuleRequest) validate() error {
	if r.Scope == "" || r.Kind == "" || r.MatchType == "" || r.Value == "" {
		return errText("scope, kind, match_type, and value are required")
	}
	if r.Scope != "global" && r.Scope != "mailbox" && r.Scope != "domain" {
		return errText("scope must be global, mailbox, or domain")
	}
	if r.Kind != "allow" && r.Kind != "block" {
		return errText("kind must be allow or block")
	}
	if r.MatchType != "email" && r.MatchType != "domain" && r.MatchType != "ip" && r.MatchType != "cidr" {
		return errText("match_type must be email, domain, ip, or cidr")
	}
	if r.Action != "" && r.Action != "junk" && r.Action != "reject" {
		return errText("action must be junk or reject")
	}
	return nil
}

type errText string

func (e errText) Error() string { return string(e) }

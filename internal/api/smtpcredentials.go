package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/dkimkey"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
	"github.com/zephyraoss/haitatsu/internal/database/ent/smtpcredential"
	"github.com/zephyraoss/haitatsu/internal/ids"
)

const maxSMTPCredentials = 50

type smtpCredentialRequest struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
}

type smtpCredentialResponse struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	Name       string     `json:"name"`
	Domains    []string   `json:"domains"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Password   string     `json:"password,omitempty"`
}

func (h *Handler) listSMTPCredentials(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.SMTPCredential.Query().Order(smtpcredential.ByCreatedAt(entsql.OrderDesc()), smtpcredential.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.SMTPCredential](smtpcredential.FieldCreatedAt, smtpcredential.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "smtp_credential_list_failed", "Failed to list SMTP credentials")
	}
	page, next := nextCursor(items, limit, func(item *ent.SMTPCredential) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	public := make([]smtpCredentialResponse, 0, len(page))
	for _, item := range page {
		public = append(public, smtpCredentialPublic(item))
	}
	return list(c, public, limit, next)
}

func (h *Handler) getSMTPCredential(c fiber.Ctx) error {
	item, err := h.client.SMTPCredential.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "smtp_credential_not_found", "SMTP credential not found")
	}
	return data(c, smtpCredentialPublic(item))
}

func (h *Handler) createSMTPCredential(c fiber.Ctx) error {
	var req smtpCredentialRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.Name == "" {
		return problem(c, fiber.StatusBadRequest, "invalid_smtp_credential", "Name is required")
	}
	domains, err := normalizeDomains(req.Domains)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_smtp_credential", err.Error())
	}
	missing, err := h.domainsWithoutDKIM(c.Context(), domains)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "dkim_lookup_failed", "Failed to look up DKIM keys")
	}
	if len(missing) > 0 {
		return problem(c, fiber.StatusUnprocessableEntity, "dkim_key_not_found", "No DKIM key configured for "+strings.Join(missing, ", "))
	}

	active, err := h.client.SMTPCredential.Query().Where(smtpcredential.RevokedAtIsNil()).Count(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "smtp_credential_count_failed", "Failed to count SMTP credentials")
	}
	if active >= maxSMTPCredentials {
		return problem(c, fiber.StatusConflict, "smtp_credential_limit_reached", fmt.Sprintf("At most %d SMTP credentials may be active", maxSMTPCredentials))
	}

	password, err := passwordauth.GeneratePassword()
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "smtp_credential_generate_failed", "Failed to generate SMTP credential password")
	}
	hash, err := passwordauth.HashPassword(password)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "smtp_credential_hash_failed", "Failed to hash SMTP credential password")
	}

	username := "smtp_" + strings.ToLower(ids.New().String())
	item, err := h.client.SMTPCredential.Create().SetUsername(username).SetName(req.Name).SetHash(hash).SetDomains(domains).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "smtp_credential_create_failed", "Failed to create SMTP credential")
	}
	if err := h.audit(c, "smtp_credential.created", "smtp_credential", item.ID, "", map[string]any{"domains": domains}); err != nil {
		return err
	}
	response := smtpCredentialPublic(item)
	response.Password = password
	return created(c, response)
}

func (h *Handler) revokeSMTPCredential(c fiber.Ctx) error {
	item, err := h.client.SMTPCredential.UpdateOneID(c.Params("id")).SetRevokedAt(time.Now()).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "smtp_credential_revoke_failed", "Failed to revoke SMTP credential")
	}
	if err := h.audit(c, "smtp_credential.revoked", "smtp_credential", item.ID, "", nil); err != nil {
		return err
	}
	return data(c, smtpCredentialPublic(item))
}

func normalizeDomains(domains []string) ([]string, error) {
	if len(domains) == 0 {
		return nil, errors.New("at least one domain is required")
	}
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if len(domain) > 253 || !domainNamePattern.MatchString(domain) {
			return nil, fmt.Errorf("domain %q is not a valid hostname", domain)
		}
		if !slices.Contains(normalized, domain) {
			normalized = append(normalized, domain)
		}
	}
	return normalized, nil
}

func (h *Handler) domainsWithoutDKIM(ctx context.Context, domains []string) ([]string, error) {
	signed, err := h.client.DKIMKey.Query().Where(dkimkey.DomainIn(domains...)).Select(dkimkey.FieldDomain).Strings(ctx)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, domain := range domains {
		if !slices.Contains(signed, domain) {
			missing = append(missing, domain)
		}
	}
	return missing, nil
}

func smtpCredentialPublic(item *ent.SMTPCredential) smtpCredentialResponse {
	return smtpCredentialResponse{
		ID:         item.ID,
		Username:   item.Username,
		Name:       item.Name,
		Domains:    item.Domains,
		LastUsedAt: item.LastUsedAt,
		RevokedAt:  item.RevokedAt,
		CreatedAt:  item.CreatedAt,
		UpdatedAt:  item.UpdatedAt,
	}
}

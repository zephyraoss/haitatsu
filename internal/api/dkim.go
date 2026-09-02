package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/dkimkey"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
)

type dkimKeyRequest struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
}

type dkimDNSResponse struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type dkimKeyResponse struct {
	ID       string          `json:"id"`
	Domain   string          `json:"domain"`
	Selector string          `json:"selector"`
	DNS      dkimDNSResponse `json:"dns"`
}

func (h *Handler) listDKIMKeys(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.DKIMKey.Query().Order(dkimkey.ByCreatedAt(entsql.OrderDesc()), dkimkey.ByID(entsql.OrderDesc())).Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.DKIMKey](dkimkey.FieldCreatedAt, dkimkey.FieldID, cur))
	}
	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "dkim_key_list_failed", "Failed to list DKIM keys")
	}
	page, next := nextCursor(items, limit, func(item *ent.DKIMKey) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	responses := make([]dkimKeyResponse, 0, len(page))
	for _, item := range page {
		responses = append(responses, dkimResponse(item))
	}
	return list(c, responses, limit, next)
}

func (h *Handler) createDKIMKey(c fiber.Ctx) error {
	var req dkimKeyRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	selector := strings.TrimSpace(req.Selector)
	if selector == "" {
		selector = "zpr1"
	}
	if domain == "" {
		return problem(c, fiber.StatusBadRequest, "domain_required", "Domain is required")
	}

	privatePEM, publicPEM, err := generateDKIMKeyPair()
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "dkim_key_generation_failed", "Failed to generate DKIM key")
	}
	item, err := h.client.DKIMKey.Create().SetDomain(domain).SetSelector(selector).SetPrivateKeyPem(privatePEM).SetPublicKeyPem(publicPEM).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "dkim_key_create_failed", "Failed to create DKIM key")
	}
	return created(c, dkimResponse(item))
}

func (h *Handler) getDKIMKey(c fiber.Ctx) error {
	item, err := h.client.DKIMKey.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "dkim_key_not_found", "DKIM key not found")
	}
	return data(c, dkimResponse(item))
}

func generateDKIMKeyPair() (string, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	privateDER := x509.MarshalPKCS1PrivateKey(key)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privateDER}))
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	return privatePEM, publicPEM, nil
}

func dkimResponse(item *ent.DKIMKey) dkimKeyResponse {
	return dkimKeyResponse{
		ID:       item.ID,
		Domain:   item.Domain,
		Selector: item.Selector,
		DNS: dkimDNSResponse{
			Name:  item.Selector + "._domainkey." + item.Domain,
			Type:  "TXT",
			Value: "v=DKIM1; k=rsa; p=" + dkimPublicKeyValue(item.PublicKeyPem),
		},
	}
}

func dkimPublicKeyValue(publicPEM string) string {
	block, _ := pem.Decode([]byte(publicPEM))
	if block == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(block.Bytes)
}

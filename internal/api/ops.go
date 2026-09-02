package api

import (
	"net"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent/dkimkey"
)

type dnsCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

func (h *Handler) reloadConfig(c fiber.Ctx) error {
	if h.reloader == nil {
		return problem(c, fiber.StatusServiceUnavailable, "reload_unavailable", "Config reload is unavailable")
	}
	if err := h.reloader.Reload(c.Context()); err != nil {
		return problem(c, fiber.StatusConflict, "reload_failed", err.Error())
	}
	return empty(c)
}

func (h *Handler) checkDNS(c fiber.Ctx) error {
	domain := strings.ToLower(strings.TrimSpace(c.Params("domain")))
	if domain == "" {
		return problem(c, fiber.StatusBadRequest, "domain_required", "Domain is required")
	}
	checks := []dnsCheck{
		h.checkMX(domain),
		h.checkDKIM(c, domain),
		checkTXT("spf", domain, "v=spf1"),
		checkTXT("dmarc", "_dmarc."+domain, "v=DMARC1"),
	}
	return data(c, fiber.Map{"domain": domain, "checks": checks})
}

func (h *Handler) checkMX(domain string) dnsCheck {
	mx, err := net.LookupMX(domain)
	if err != nil || len(mx) == 0 {
		return dnsCheck{Name: "mx", Status: "missing", Message: "No MX records found"}
	}
	for _, record := range mx {
		if strings.TrimSuffix(strings.ToLower(record.Host), ".") == strings.ToLower(h.config.Get().Server.PublicHostname) {
			return dnsCheck{Name: "mx", Status: "ok", Message: "MX points to configured inbound host"}
		}
	}
	return dnsCheck{Name: "mx", Status: "warning", Message: "MX records do not point to configured inbound host"}
}

func (h *Handler) checkDKIM(c fiber.Ctx, domain string) dnsCheck {
	key, err := h.client.DKIMKey.Query().Where(dkimkey.DomainEQ(domain)).First(c.Context())
	if err != nil {
		return dnsCheck{Name: "dkim", Status: "missing", Message: "No DKIM key generated for domain"}
	}
	name := key.Selector + "._domainkey." + domain
	records, err := net.LookupTXT(name)
	if err != nil || len(records) == 0 {
		return dnsCheck{Name: "dkim", Status: "missing", Message: "No DKIM TXT record found"}
	}
	for _, record := range records {
		if strings.Contains(record, "v=DKIM1") {
			return dnsCheck{Name: "dkim", Status: "ok", Message: "DKIM TXT record exists"}
		}
	}
	return dnsCheck{Name: "dkim", Status: "warning", Message: "DKIM TXT record does not look valid"}
}

func checkTXT(name string, domain string, prefix string) dnsCheck {
	records, err := net.LookupTXT(domain)
	if err != nil || len(records) == 0 {
		return dnsCheck{Name: name, Status: "missing", Message: "TXT record not found"}
	}
	for _, record := range records {
		if strings.HasPrefix(record, prefix) {
			return dnsCheck{Name: name, Status: "ok", Message: "TXT record exists"}
		}
	}
	return dnsCheck{Name: name, Status: "warning", Message: "TXT record exists but expected policy was not found"}
}

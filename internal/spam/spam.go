package spam

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/mail"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/senderrule"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

type SMTPContext struct {
	MailFrom string
	RemoteIP string
	HELO     string
	TLS      bool
}

type Assessment struct {
	Score       float64
	Reasons     []string
	AuthResults map[string]any
	Header      string
	Junk        bool
	Reject      bool
}

type Checker struct {
	client *ent.Client
	cfg    config.SpamConfig
	authID string
}

func NewChecker(client *ent.Client, cfg config.SpamConfig, authID string) *Checker {
	return &Checker{client: client, cfg: cfg, authID: authID}
}

func (c *Checker) Check(ctx context.Context, raw []byte, smtp SMTPContext, recipients []routing.Result) Assessment {
	metadata := mailparse.Parse(raw)
	fromDomain := firstAddressDomain(metadata.From)
	dkimResult, dkimDomain := verifyDKIM(raw)
	spfResult, spfDomain, spfReason := checkSPF(ctx, smtp)
	dmarcResult, dmarcPolicy := checkDMARC(fromDomain, dkimResult, dkimDomain, spfResult, spfDomain)
	listKind, listAction := c.senderRuleMatch(ctx, metadata, smtp, recipients)

	score, reasons := score(spfResult, dkimResult, dmarcResult, dmarcPolicy, listKind)
	if listKind == "allow" {
		score -= 5
	}
	if score < 0 {
		score = 0
	}

	assessment := Assessment{
		Score:   score,
		Reasons: reasons,
		AuthResults: map[string]any{
			"spf":          string(spfResult),
			"spf_domain":   spfDomain,
			"spf_reason":   spfReason,
			"dkim":         string(dkimResult),
			"dkim_domain":  dkimDomain,
			"dmarc":        string(dmarcResult),
			"dmarc_policy": string(dmarcPolicy),
			"tls":          smtp.TLS,
			"remote_ip":    smtp.RemoteIP,
			"helo":         smtp.HELO,
			"spam_reasons": reasons,
			"list_kind":    listKind,
			"list_action":  listAction,
		},
	}
	assessment.Header = c.authResultsHeader(spfResult, dkimResult, dmarcResult, smtp, metadata)
	dmarcFailed := dmarcResult == authres.ResultFail
	assessment.Junk = score >= c.junkThreshold() || (dmarcFailed && dmarcPolicy == dmarc.PolicyQuarantine) || listKind == "block"
	assessment.Reject = score >= c.rejectThreshold() || (dmarcFailed && dmarcPolicy == dmarc.PolicyReject) || listAction == "reject"
	return assessment
}

func (c *Checker) authResultsHeader(spfResult authres.ResultValue, dkimResult authres.ResultValue, dmarcResult authres.ResultValue, smtp SMTPContext, metadata mailparse.Metadata) string {
	mailFromDomain := addressDomain(smtp.MailFrom)
	if mailFromDomain == "" {
		mailFromDomain = smtp.MailFrom
	}
	return authres.Format(c.authID, []authres.Result{
		&authres.SPFResult{Value: spfResult, From: mailFromDomain, Helo: smtp.HELO},
		&authres.DKIMResult{Value: dkimResult},
		&authres.DMARCResult{Value: dmarcResult, From: firstAddressDomain(metadata.From)},
	})
}

func (c *Checker) senderRuleMatch(ctx context.Context, metadata mailparse.Metadata, smtp SMTPContext, recipients []routing.Result) (string, string) {
	entries, err := c.client.SenderRule.Query().Where(senderrule.ScopeEQ("global")).All(ctx)
	if err != nil {
		return "", ""
	}
	mailboxIDs := recipientMailboxIDs(recipients)
	for _, id := range mailboxIDs {
		items, err := c.client.SenderRule.Query().Where(senderrule.ScopeEQ("mailbox"), senderrule.ScopeRefEQ(id)).All(ctx)
		if err == nil {
			entries = append(entries, items...)
		}
	}
	for _, entry := range entries {
		if matches(entry, metadata, smtp) {
			return entry.Kind, entry.Action
		}
	}
	return "", ""
}

func verifyDKIM(raw []byte) (authres.ResultValue, string) {
	verifications, err := dkim.Verify(bytes.NewReader(raw))
	if err != nil || len(verifications) == 0 {
		return authres.ResultNone, ""
	}
	for _, verification := range verifications {
		if verification.Err == nil {
			return authres.ResultPass, verification.Domain
		}
	}
	return authres.ResultFail, verifications[0].Domain
}

func checkDMARC(fromDomain string, dkimResult authres.ResultValue, dkimDomain string, spfResult authres.ResultValue, spfDomain string) (authres.ResultValue, dmarc.Policy) {
	if fromDomain == "" {
		return authres.ResultNone, dmarc.PolicyNone
	}
	record, err := dmarc.Lookup(fromDomain)
	if err != nil {
		if errors.Is(err, dmarc.ErrNoPolicy) {
			return authres.ResultNone, dmarc.PolicyNone
		}
		return authres.ResultTempError, dmarc.PolicyNone
	}
	if dkimResult == authres.ResultPass && strings.EqualFold(fromDomain, dkimDomain) {
		return authres.ResultPass, record.Policy
	}
	if spfResult == authres.ResultPass && strings.EqualFold(fromDomain, spfDomain) {
		return authres.ResultPass, record.Policy
	}
	return authres.ResultFail, record.Policy
}

func score(spfResult authres.ResultValue, dkimResult authres.ResultValue, dmarcResult authres.ResultValue, policy dmarc.Policy, listKind string) (float64, []string) {
	var value float64
	var reasons []string
	add := func(points float64, reason string) {
		value += points
		reasons = append(reasons, reason)
	}
	if spfResult == authres.ResultFail {
		add(2, "spf_fail")
	}
	if spfResult == authres.ResultSoftFail {
		add(1, "spf_softfail")
	}
	if dkimResult == authres.ResultFail {
		add(2, "dkim_fail")
	}
	if dmarcResult == authres.ResultFail {
		add(3, "dmarc_fail")
		if policy == dmarc.PolicyQuarantine {
			add(2, "dmarc_quarantine")
		}
		if policy == dmarc.PolicyReject {
			add(10, "dmarc_reject")
		}
	}
	if listKind == "block" {
		add(10, "blocklist")
	}
	return value, reasons
}

func matches(entry *ent.SenderRule, metadata mailparse.Metadata, smtp SMTPContext) bool {
	value := strings.ToLower(entry.Value)
	switch entry.MatchType {
	case "email":
		for _, address := range metadata.From {
			if strings.EqualFold(address, value) {
				return true
			}
		}
	case "domain":
		return strings.EqualFold(firstAddressDomain(metadata.From), value)
	case "ip", "cidr":
		return ipMatches(smtp.RemoteIP, value)
	}
	return false
}

func ipMatches(remoteIP string, value string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	if strings.Contains(value, "/") {
		_, network, err := net.ParseCIDR(value)
		return err == nil && network.Contains(ip)
	}
	return ip.Equal(net.ParseIP(value))
}

func firstAddressDomain(addresses []string) string {
	if len(addresses) == 0 {
		return ""
	}
	return addressDomain(addresses[0])
}

func addressDomain(address string) string {
	parsed, err := mail.ParseAddress(address)
	if err == nil {
		address = parsed.Address
	}
	_, domain, _ := strings.Cut(strings.ToLower(address), "@")
	return domain
}

func recipientMailboxIDs(recipients []routing.Result) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, recipient := range recipients {
		for _, mailbox := range recipient.Mailboxes {
			if _, ok := seen[mailbox.ID]; ok {
				continue
			}
			seen[mailbox.ID] = struct{}{}
			ids = append(ids, mailbox.ID)
		}
	}
	return ids
}

func (c *Checker) junkThreshold() float64 {
	if c.cfg.JunkThreshold == 0 {
		return 5
	}
	return c.cfg.JunkThreshold
}

func (c *Checker) rejectThreshold() float64 {
	if c.cfg.RejectThreshold == 0 {
		return 10
	}
	return c.cfg.RejectThreshold
}

package spam

import (
	"context"
	"maps"
	"math"
	"slices"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

// MailboxAssessment records initial placement before routing rules run.
type MailboxAssessment struct {
	Junk          bool            `json:"junk"`
	JunkThreshold float64         `json:"junk_threshold"`
	Reasons       []string        `json:"spam_reasons"`
	TypeSafe      *TypeSafeResult `json:"typesafe,omitempty"`
}

// JunkForMailbox also supports callers that supply a message-wide assessment.
func (a Assessment) JunkForMailbox(id string) bool {
	if mailbox, ok := a.Mailboxes[id]; ok {
		return mailbox.Junk
	}
	return a.Junk
}

func (c *Checker) applyMailboxPolicy(ctx context.Context, raw []byte, cfg config.SpamConfig, recipients []routing.Result, forcedJunk bool, a *Assessment) {
	typeSafeCfg := cfg.TypeSafe.WithDefaults()
	a.Mailboxes = make(map[string]MailboxAssessment)
	configs := make(map[string]config.TypeSafeConfig)
	allJunk := true
	// Capture each effective setting before the evaluator can run or config reload.
	for _, recipient := range recipients {
		for _, mailbox := range recipient.Mailboxes {
			if _, exists := a.Mailboxes[mailbox.ID]; exists {
				continue
			}
			threshold := junkThreshold(cfg)
			if v, ok := mailbox.SpamThresholds["junk_threshold"]; ok && finite(v) && v >= 0 {
				threshold = v
			}
			localCfg := typeSafeCfg
			if v, ok := mailbox.SpamThresholds["spam_threshold"]; ok && finite(v) && v > 0 && v <= 1 {
				localCfg.SpamThreshold = v
			}
			if v, ok := mailbox.SpamThresholds["phishing_threshold"]; ok && finite(v) && v > 0 && v <= 1 {
				localCfg.PhishingThreshold = v
			}
			local := MailboxAssessment{Junk: forcedJunk || a.Score >= threshold, JunkThreshold: threshold, Reasons: slices.Clone(a.Reasons)}
			a.Mailboxes[mailbox.ID], configs[mailbox.ID] = local, localCfg
			allJunk = allJunk && local.Junk
		}
	}
	if len(a.Mailboxes) == 0 {
		c.applyTypeSafe(ctx, raw, typeSafeCfg, a)
		return
	}

	// Reuse the existing extraction, validation and failure handling once per
	// message. The probe is eligible if any destination still needs evaluation.
	probe := *a
	probe.Junk = allJunk
	probe.Reasons = slices.Clone(a.Reasons)
	probe.AuthResults = maps.Clone(a.AuthResults)
	evaluator := *c
	evaluator.observeTypeSafe = nil
	evaluator.applyTypeSafe(ctx, raw, typeSafeCfg, &probe)
	shared, enabled := probe.AuthResults["typesafe"].(TypeSafeResult)
	if enabled {
		global := recipientTypeSafeResult(shared, typeSafeCfg, a.Junk, a.Reject)
		applyRecipientTypeSafe(a, global)
		observed := shared
		observed.WouldJunk, observed.AppliedJunk = false, false
		for id, local := range a.Mailboxes {
			result := recipientTypeSafeResult(shared, configs[id], local.Junk, a.Reject)
			local.TypeSafe = &result
			localAssessment := Assessment{Junk: local.Junk, Reasons: local.Reasons}
			applyRecipientTypeSafe(&localAssessment, result)
			local.Junk, local.Reasons = localAssessment.Junk, localAssessment.Reasons
			a.Mailboxes[id] = local
			observed.WouldJunk = observed.WouldJunk || result.WouldJunk
			observed.AppliedJunk = observed.AppliedJunk || result.AppliedJunk
		}
		// Count requests and token usage once. Decision counters indicate whether
		// TypeSafe selected Junk for any recipient of this message.
		if c.observeTypeSafe != nil {
			c.observeTypeSafe(observed)
		}
	}
	if a.AuthResults == nil {
		a.AuthResults = make(map[string]any)
	}
	a.AuthResults["mailboxes"] = a.Mailboxes
}

func recipientTypeSafeResult(shared TypeSafeResult, cfg config.TypeSafeConfig, junk, reject bool) TypeSafeResult {
	result := shared
	result.SpamThreshold, result.PhishingThreshold = cfg.SpamThreshold, cfg.PhishingThreshold
	result.WouldJunk, result.AppliedJunk = false, false
	switch {
	case reject:
		result.Status = "skipped_existing_reject"
	case junk:
		result.Status = "skipped_existing_junk"
	case result.Status == "ok":
		result.WouldJunk = *result.SpamProbability >= cfg.SpamThreshold || *result.PhishingProbability >= cfg.PhishingThreshold
		result.AppliedJunk = cfg.Mode == "junk" && result.WouldJunk
	}
	return result
}

func applyRecipientTypeSafe(a *Assessment, result TypeSafeResult) {
	if result.AppliedJunk {
		a.Junk = true
		if *result.SpamProbability >= result.SpamThreshold {
			a.Reasons = append(a.Reasons, "typesafe_spam")
		}
		if *result.PhishingProbability >= result.PhishingThreshold {
			a.Reasons = append(a.Reasons, "typesafe_phishing")
		}
	}
	if a.AuthResults == nil {
		a.AuthResults = make(map[string]any)
	}
	a.AuthResults["typesafe"] = result
	if result.AppliedJunk {
		a.AuthResults["spam_reasons"] = a.Reasons
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

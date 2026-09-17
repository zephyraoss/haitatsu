package spam

import (
	"context"
	"math"

	"github.com/zephyraoss/haitatsu/internal/config"
)

const TypeSafeSchemaVersion = "1"

// TypeSafeResult records the decision at initial delivery, before routing rules.
type TypeSafeResult struct {
	SchemaVersion       string            `json:"schema_version"`
	QuestionVersion     string            `json:"question_version"`
	ExtractorVersion    string            `json:"extractor_version"`
	Mode                string            `json:"mode"`
	RequestedModel      string            `json:"requested_model"`
	Model               string            `json:"model,omitempty"`
	Status              string            `json:"status"`
	SpamProbability     *float64          `json:"spam_probability"`
	PhishingProbability *float64          `json:"phishing_probability"`
	SpamThreshold       float64           `json:"spam_threshold"`
	PhishingThreshold   float64           `json:"phishing_threshold"`
	WouldJunk           bool              `json:"would_junk"`
	AppliedJunk         bool              `json:"applied_junk"`
	ElapsedMS           int64             `json:"elapsed_ms"`
	Input               TypeSafeInputInfo `json:"input"`
	Usage               *TypeSafeUsage    `json:"usage,omitempty"`
}

func (c *Checker) applyTypeSafe(ctx context.Context, raw []byte, cfg config.TypeSafeConfig, assessment *Assessment) {
	if cfg.Mode != "shadow" && cfg.Mode != "junk" {
		return
	}
	result := TypeSafeResult{
		SchemaVersion: TypeSafeSchemaVersion, QuestionVersion: TypeSafeQuestionVersion, ExtractorVersion: TypeSafeExtractorVersion,
		Mode: cfg.Mode, RequestedModel: cfg.Model, SpamThreshold: cfg.SpamThreshold, PhishingThreshold: cfg.PhishingThreshold,
		Input: TypeSafeInputInfo{Status: "not_extracted"},
	}
	switch {
	case assessment.Reject:
		result.Status = "skipped_existing_reject"
	case assessment.Junk:
		result.Status = "skipped_existing_junk"
	case c.typesafe == nil:
		result.Status = "skipped_unavailable"
	default:
		state, info := BuildTypeSafeState(raw, cfg.MaxTextBytes, assessment.AuthResults)
		result.Input = info
		if info.Status != "ok" && info.Status != "partial" {
			result.Status = info.Status
			break
		}
		evaluation := c.typesafe.Evaluate(ctx, cfg, state)
		result.Status, result.Model, result.ElapsedMS = evaluation.Status, evaluation.Model, evaluation.ElapsedMS
		result.Usage = evaluation.Usage
		if evaluation.Status == "ok" {
			if !validTypeSafeProbability(evaluation.SpamProbability) || !validTypeSafeProbability(evaluation.PhishingProbability) {
				result.Status = "error_invalid_response"
				break
			}
			result.SpamProbability, result.PhishingProbability = evaluation.SpamProbability, evaluation.PhishingProbability
			spam := *result.SpamProbability >= cfg.SpamThreshold
			phishing := *result.PhishingProbability >= cfg.PhishingThreshold
			result.WouldJunk = spam || phishing
			result.AppliedJunk = cfg.Mode == "junk" && result.WouldJunk
			if result.AppliedJunk {
				assessment.Junk = true
				if spam {
					assessment.Reasons = append(assessment.Reasons, "typesafe_spam")
				}
				if phishing {
					assessment.Reasons = append(assessment.Reasons, "typesafe_phishing")
				}
			}
		}
	}
	if assessment.AuthResults == nil {
		assessment.AuthResults = make(map[string]any)
	}
	assessment.AuthResults["typesafe"] = result
	if result.AppliedJunk {
		assessment.AuthResults["spam_reasons"] = assessment.Reasons
	}
	if c.observeTypeSafe != nil {
		c.observeTypeSafe(result)
	}
}

func validTypeSafeProbability(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= 1
}

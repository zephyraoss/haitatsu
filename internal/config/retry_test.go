package config

import (
	"testing"
	"time"
)

func TestWebhookRetryPolicyDefaults(t *testing.T) {
	policy := WebhookConfig{}.RetryPolicy()
	if policy.MaxAttempts != 10 || policy.MaxDelay != time.Hour {
		t.Fatalf("unexpected defaults %+v", policy)
	}
	if !policy.Exhausted(10) || policy.Exhausted(9) {
		t.Fatal("exhaustion boundary wrong")
	}
}

func TestReloadImpactAllowsRelayAndLimitsChanges(t *testing.T) {
	base := validConfig()
	next := validConfig()
	next.Relay.Password = "rotated"
	next.Limits.DefaultOutboundPerHour = 50
	next.Spam.JunkThreshold = 3
	next.API.ServiceToken = "new-token"
	if impact := base.ReloadImpact(&next); impact.RequiresRestart() {
		t.Fatalf("expected reloadable, got %v", impact.StructuralChanges)
	}
	next.Workers.Enabled = true
	if impact := base.ReloadImpact(&next); !impact.RequiresRestart() {
		t.Fatal("toggling workers should require restart")
	}
}

func TestLimitFallbacks(t *testing.T) {
	cfg := validConfig()
	if cfg.InboundMessageSize() != 50*1024*1024 || cfg.InboundRecipients() != 100 || cfg.ConnectionsPerIP() != 20 {
		t.Fatal("defaults not applied")
	}
	cfg.Limits.MaxMessageSizeBytes = 1024
	cfg.SMTP.MaxMessageSizeBytes = 2048
	if cfg.InboundMessageSize() != 1024 {
		t.Fatal("limits block should win over smtp block")
	}
}

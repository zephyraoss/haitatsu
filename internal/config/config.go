package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/apple/pkl-go/pkl"
)

type Config struct {
	Server        ServerConfig       `pkl:"server" json:"server"`
	SMTP          SMTPConfig         `pkl:"smtp" json:"smtp"`
	IMAP          IMAPConfig         `pkl:"imap" json:"imap"`
	Submission    SubmissionConfig   `pkl:"submission" json:"submission"`
	Relay         RelayConfig        `pkl:"relay" json:"relay"`
	Postgres      PostgresConfig     `pkl:"postgres" json:"postgres"`
	S3            S3Config           `pkl:"s3" json:"s3"`
	Logging       LoggingConfig      `pkl:"logging" json:"logging"`
	Metrics       MetricsConfig      `pkl:"metrics" json:"metrics"`
	API           APIConfig          `pkl:"api" json:"api"`
	Workers       WorkersConfig      `pkl:"workers" json:"workers"`
	TLS           TLSConfig          `pkl:"tls" json:"tls"`
	Webhooks      WebhookConfig      `pkl:"webhooks" json:"webhooks"`
	Notifications NotificationConfig `pkl:"notifications" json:"notifications"`
	Spam          SpamConfig         `pkl:"spam" json:"spam"`
	Limits        LimitsConfig       `pkl:"limits" json:"limits"`
}

type ServerConfig struct {
	APIAddr                string `pkl:"api_addr" json:"api_addr"`
	PublicHostname         string `pkl:"public_hostname" json:"public_hostname"`
	InstanceName           string `pkl:"instance_name" json:"instance_name"`
	ShutdownTimeoutSeconds int    `pkl:"shutdown_timeout_seconds" json:"shutdown_timeout_seconds"`
}

type SMTPConfig struct {
	InboundAddr          string `pkl:"inbound_addr" json:"inbound_addr"`
	MaxMessageSizeBytes  int64  `pkl:"max_message_size_bytes" json:"max_message_size_bytes"`
	MaxInboundRecipients int    `pkl:"max_inbound_recipients" json:"max_inbound_recipients"`
}

type IMAPConfig struct {
	Addr                string `pkl:"addr" json:"addr"`
	MaxConnectionsPerIP int    `pkl:"max_connections_per_ip" json:"max_connections_per_ip"`
}

type SubmissionConfig struct {
	StartTLSAddr string `pkl:"starttls_addr" json:"starttls_addr"`
	TLSAddr      string `pkl:"tls_addr" json:"tls_addr"`
}

type RelayConfig struct {
	Addr            string `pkl:"addr" json:"addr"`
	Username        string `pkl:"username" json:"username"`
	Password        string `pkl:"password" json:"password"`
	FromHost        string `pkl:"from_host" json:"from_host"`
	MaxAttempts     int    `pkl:"max_attempts" json:"max_attempts"`
	MaxRetryMinutes int    `pkl:"max_retry_minutes" json:"max_retry_minutes"`
}

func (c RelayConfig) RetryPolicy() RetryPolicy {
	policy := RetryPolicy{MaxAttempts: c.MaxAttempts, MaxDelay: time.Duration(c.MaxRetryMinutes) * time.Minute}
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 30
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = 4 * time.Hour
	}
	return policy
}

type RetryPolicy struct {
	MaxAttempts int
	MaxDelay    time.Duration
}

func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Minute
	for range attempt {
		delay *= 2
		if delay >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return delay
}

func (p RetryPolicy) Exhausted(attempts int) bool {
	return attempts >= p.MaxAttempts
}

func (c ServerConfig) ShutdownTimeout() time.Duration {
	if c.ShutdownTimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.ShutdownTimeoutSeconds) * time.Second
}

type PostgresConfig struct {
	DSN string `pkl:"dsn" json:"dsn"`
}

type S3Config struct {
	Endpoint        string `pkl:"endpoint" json:"endpoint"`
	Region          string `pkl:"region" json:"region"`
	Bucket          string `pkl:"bucket" json:"bucket"`
	AccessKeyID     string `pkl:"access_key_id" json:"access_key_id"`
	SecretAccessKey string `pkl:"secret_access_key" json:"secret_access_key"`
	UseSSL          bool   `pkl:"use_ssl" json:"use_ssl"`
}

type LoggingConfig struct {
	Level        string `pkl:"level" json:"level"`
	AxiomEnabled bool   `pkl:"axiom_enabled" json:"axiom_enabled"`
	AxiomDataset string `pkl:"axiom_dataset" json:"axiom_dataset"`
	AxiomToken   string `pkl:"axiom_token" json:"axiom_token"`
	AxiomURL     string `pkl:"axiom_url" json:"axiom_url"`
}

type MetricsConfig struct {
	Enabled bool `pkl:"enabled" json:"enabled"`
}

type APIConfig struct {
	ServiceToken string `pkl:"service_token" json:"service_token"`
}

type WorkersConfig struct {
	Enabled     bool `pkl:"enabled" json:"enabled"`
	Concurrency int  `pkl:"concurrency" json:"concurrency"`
}

type TLSConfig struct {
	Mode                          string `pkl:"mode" json:"mode"`
	CertFile                      string `pkl:"cert_file" json:"cert_file"`
	KeyFile                       string `pkl:"key_file" json:"key_file"`
	ACMEEmail                     string `pkl:"acme_email" json:"acme_email"`
	ACMECA                        string `pkl:"acme_ca" json:"acme_ca"`
	ACMECachePath                 string `pkl:"acme_cache_path" json:"acme_cache_path"`
	ACMEListenHost                string `pkl:"acme_listen_host" json:"acme_listen_host"`
	ACMEHTTPPort                  int    `pkl:"acme_http_port" json:"acme_http_port"`
	ACMETLSALPNPort               int    `pkl:"acme_tls_alpn_port" json:"acme_tls_alpn_port"`
	ACMEDisableHTTPChallenge      bool   `pkl:"acme_disable_http_challenge" json:"acme_disable_http_challenge"`
	ACMEDisableTLSALPNChallenge   bool   `pkl:"acme_disable_tls_alpn_challenge" json:"acme_disable_tls_alpn_challenge"`
	ACMEDisableDistributedSolvers bool   `pkl:"acme_disable_distributed_solvers" json:"acme_disable_distributed_solvers"`
	ACMEDNSProvider               string `pkl:"acme_dns_provider" json:"acme_dns_provider"`
	ACMECloudflareAPIToken        string `pkl:"acme_cloudflare_api_token" json:"acme_cloudflare_api_token"`
	ACMEStorage                   string `pkl:"acme_storage" json:"acme_storage"`
	ACMES3Prefix                  string `pkl:"acme_s3_prefix" json:"acme_s3_prefix"`
}

type WebhookConfig struct {
	DefaultTimeoutSeconds int               `pkl:"default_timeout_seconds" json:"default_timeout_seconds"`
	Secret                string            `pkl:"secret" json:"secret"`
	Endpoints             map[string]string `pkl:"endpoints" json:"endpoints"`
	MaxAttempts           int               `pkl:"max_attempts" json:"max_attempts"`
}

func (c WebhookConfig) RetryPolicy() RetryPolicy {
	policy := RetryPolicy{MaxAttempts: c.MaxAttempts, MaxDelay: time.Hour}
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 10
	}
	return policy
}

type NotificationConfig struct {
	FromAddress      string `pkl:"from_address" json:"from_address"`
	RenderURL        string `pkl:"render_url" json:"render_url"`
	RenderSecret     string `pkl:"render_secret" json:"render_secret"`
	TimeoutSeconds   int    `pkl:"timeout_seconds" json:"timeout_seconds"`
	MaxResponseBytes int64  `pkl:"max_response_bytes" json:"max_response_bytes"`
}

type SpamConfig struct {
	JunkThreshold   float64  `pkl:"junk_threshold" json:"junk_threshold"`
	RejectThreshold float64  `pkl:"reject_threshold" json:"reject_threshold"`
	DNSBLZones      []string `pkl:"dnsbl_zones" json:"dnsbl_zones"`
	DNSBLScore      float64  `pkl:"dnsbl_score" json:"dnsbl_score"`
	RequireHELO     bool     `pkl:"require_helo" json:"require_helo"`
}

type LimitsConfig struct {
	MaxMessageSizeBytes           int64 `pkl:"max_message_size_bytes" json:"max_message_size_bytes"`
	MaxInboundRecipients          int   `pkl:"max_inbound_recipients" json:"max_inbound_recipients"`
	MaxSubmissionRecipients       int   `pkl:"max_submission_recipients" json:"max_submission_recipients"`
	MaxConnectionsPerIP           int   `pkl:"max_connections_per_ip" json:"max_connections_per_ip"`
	InboundMessagesPerMinutePerIP int   `pkl:"inbound_messages_per_minute_per_ip" json:"inbound_messages_per_minute_per_ip"`
	DefaultOutboundPerHour        int64 `pkl:"default_outbound_per_hour" json:"default_outbound_per_hour"`
	DefaultOutboundPerDay         int64 `pkl:"default_outbound_per_day" json:"default_outbound_per_day"`
	DefaultOutboundRecipients     int64 `pkl:"default_outbound_recipients_per_message" json:"default_outbound_recipients_per_message"`
}

func (c Config) InboundMessageSize() int64 {
	if c.Limits.MaxMessageSizeBytes > 0 {
		return c.Limits.MaxMessageSizeBytes
	}
	if c.SMTP.MaxMessageSizeBytes > 0 {
		return c.SMTP.MaxMessageSizeBytes
	}
	return 50 * 1024 * 1024
}

func (c Config) InboundRecipients() int {
	if c.Limits.MaxInboundRecipients > 0 {
		return c.Limits.MaxInboundRecipients
	}
	if c.SMTP.MaxInboundRecipients > 0 {
		return c.SMTP.MaxInboundRecipients
	}
	return 100
}

func (c Config) SubmissionRecipients() int {
	if c.Limits.MaxSubmissionRecipients > 0 {
		return c.Limits.MaxSubmissionRecipients
	}
	return 100
}

func (c Config) ConnectionsPerIP() int {
	if c.Limits.MaxConnectionsPerIP > 0 {
		return c.Limits.MaxConnectionsPerIP
	}
	return 20
}

func (c Config) IMAPConnectionsPerIP() int {
	if c.IMAP.MaxConnectionsPerIP > 0 {
		return c.IMAP.MaxConnectionsPerIP
	}
	return 30
}

func (c Config) InboundMessagesPerMinute() int {
	if c.Limits.InboundMessagesPerMinutePerIP > 0 {
		return c.Limits.InboundMessagesPerMinutePerIP
	}
	return 120
}

type ReloadImpact struct {
	StructuralChanges []string
}

func Load(ctx context.Context, path string) (*Config, error) {
	evaluator, err := pkl.NewEvaluator(ctx, pkl.PreconfiguredOptions, func(options *pkl.EvaluatorOptions) {
		options.OutputFormat = "json"
	})
	if err != nil {
		return nil, fmt.Errorf("create pkl evaluator: %w", err)
	}
	defer evaluator.Close()

	var cfg Config
	output, err := evaluator.EvaluateOutputText(ctx, pkl.FileSource(path))
	if err != nil {
		return nil, fmt.Errorf("evaluate pkl module: %w", err)
	}
	if err := json.Unmarshal([]byte(output), &cfg); err != nil {
		return nil, fmt.Errorf("decode pkl json: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Server.APIAddr) == "" {
		problems = append(problems, "server.api_addr is required")
	}
	if strings.TrimSpace(c.Server.PublicHostname) == "" {
		problems = append(problems, "server.public_hostname is required")
	}
	if strings.TrimSpace(c.Server.InstanceName) == "" {
		problems = append(problems, "server.instance_name is required")
	}
	if strings.TrimSpace(c.SMTP.InboundAddr) == "" {
		problems = append(problems, "smtp.inbound_addr is required")
	}
	if strings.TrimSpace(c.IMAP.Addr) == "" {
		problems = append(problems, "imap.addr is required")
	}
	if strings.TrimSpace(c.Submission.StartTLSAddr) == "" {
		problems = append(problems, "submission.starttls_addr is required")
	}
	if strings.TrimSpace(c.Submission.TLSAddr) == "" {
		problems = append(problems, "submission.tls_addr is required")
	}
	if strings.TrimSpace(c.Relay.Addr) == "" {
		problems = append(problems, "relay.addr is required")
	}
	if c.SMTP.MaxMessageSizeBytes < 0 {
		problems = append(problems, "smtp.max_message_size_bytes must be >= 0")
	}
	if c.SMTP.MaxInboundRecipients < 0 {
		problems = append(problems, "smtp.max_inbound_recipients must be >= 0")
	}
	if strings.TrimSpace(c.Postgres.DSN) == "" {
		problems = append(problems, "postgres.dsn is required")
	}
	if strings.TrimSpace(c.S3.Endpoint) == "" {
		problems = append(problems, "s3.endpoint is required")
	}
	if strings.TrimSpace(c.S3.Bucket) == "" {
		problems = append(problems, "s3.bucket is required")
	}
	if strings.TrimSpace(c.S3.AccessKeyID) == "" {
		problems = append(problems, "s3.access_key_id is required")
	}
	if strings.TrimSpace(c.S3.SecretAccessKey) == "" {
		problems = append(problems, "s3.secret_access_key is required")
	}
	if c.Workers.Concurrency < 0 {
		problems = append(problems, "workers.concurrency must be >= 0")
	}
	if c.Logging.AxiomEnabled {
		if strings.TrimSpace(c.Logging.AxiomDataset) == "" {
			problems = append(problems, "logging.axiom_dataset is required when Axiom logging is enabled")
		}
		if strings.TrimSpace(c.Logging.AxiomToken) == "" {
			problems = append(problems, "logging.axiom_token is required when Axiom logging is enabled")
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.TLS.Mode)) {
	case "", "manual", "acme", "off", "disabled":
	default:
		problems = append(problems, "tls.mode must be manual, acme, off, or disabled")
	}
	if strings.EqualFold(strings.TrimSpace(c.TLS.Mode), "acme") && strings.TrimSpace(c.TLS.ACMEEmail) == "" {
		problems = append(problems, "tls.acme_email is required when ACME TLS is enabled")
	}
	switch strings.ToLower(strings.TrimSpace(c.TLS.ACMEDNSProvider)) {
	case "", "cloudflare":
	default:
		problems = append(problems, "tls.acme_dns_provider must be empty or cloudflare")
	}
	if strings.EqualFold(strings.TrimSpace(c.TLS.ACMEDNSProvider), "cloudflare") && strings.TrimSpace(c.TLS.ACMECloudflareAPIToken) == "" {
		problems = append(problems, "tls.acme_cloudflare_api_token is required when tls.acme_dns_provider is cloudflare")
	}
	switch strings.ToLower(strings.TrimSpace(c.TLS.ACMEStorage)) {
	case "", "file", "s3":
	default:
		problems = append(problems, "tls.acme_storage must be file or s3")
	}
	if c.TLS.ACMEHTTPPort < 0 {
		problems = append(problems, "tls.acme_http_port must be >= 0")
	}
	if c.TLS.ACMETLSALPNPort < 0 {
		problems = append(problems, "tls.acme_tls_alpn_port must be >= 0")
	}
	if c.Limits.MaxMessageSizeBytes < 0 {
		problems = append(problems, "limits.max_message_size_bytes must be >= 0")
	}
	if c.Limits.MaxInboundRecipients < 0 {
		problems = append(problems, "limits.max_inbound_recipients must be >= 0")
	}
	if c.Limits.MaxSubmissionRecipients < 0 || c.Limits.MaxConnectionsPerIP < 0 || c.Limits.InboundMessagesPerMinutePerIP < 0 {
		problems = append(problems, "limits values must be >= 0")
	}
	if c.Limits.DefaultOutboundPerHour < 0 || c.Limits.DefaultOutboundPerDay < 0 || c.Limits.DefaultOutboundRecipients < 0 {
		problems = append(problems, "limits.default_outbound_* must be >= 0")
	}
	if c.IMAP.MaxConnectionsPerIP < 0 {
		problems = append(problems, "imap.max_connections_per_ip must be >= 0")
	}
	if c.Relay.MaxAttempts < 0 || c.Relay.MaxRetryMinutes < 0 {
		problems = append(problems, "relay.max_attempts and relay.max_retry_minutes must be >= 0")
	}
	if c.Webhooks.MaxAttempts < 0 {
		problems = append(problems, "webhooks.max_attempts must be >= 0")
	}
	if c.Spam.DNSBLScore < 0 {
		problems = append(problems, "spam.dnsbl_score must be >= 0")
	}
	if len(c.Webhooks.Endpoints) > 0 && strings.TrimSpace(c.Webhooks.Secret) == "" {
		problems = append(problems, "webhooks.secret is required when webhook endpoints are configured")
	}
	if strings.TrimSpace(c.Notifications.FromAddress) != "" {
		if _, err := mail.ParseAddress(c.Notifications.FromAddress); err != nil {
			problems = append(problems, "notifications.from_address must be a valid email address")
		}
	}
	if c.Notifications.TimeoutSeconds < 0 {
		problems = append(problems, "notifications.timeout_seconds must be >= 0")
	}
	if c.Notifications.MaxResponseBytes < 0 {
		problems = append(problems, "notifications.max_response_bytes must be >= 0")
	}
	if c.Spam.JunkThreshold < 0 {
		problems = append(problems, "spam.junk_threshold must be >= 0")
	}
	if c.Spam.RejectThreshold < 0 {
		problems = append(problems, "spam.reject_threshold must be >= 0")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (c Config) ReloadImpact(next *Config) ReloadImpact {
	if next == nil {
		return ReloadImpact{StructuralChanges: []string{"config is nil"}}
	}

	var changes []string
	if c.Postgres != next.Postgres {
		changes = append(changes, "postgres")
	}
	if c.S3 != next.S3 {
		changes = append(changes, "s3")
	}
	if c.Server.APIAddr != next.Server.APIAddr || c.Server.PublicHostname != next.Server.PublicHostname || c.Server.InstanceName != next.Server.InstanceName {
		changes = append(changes, "server identity/listener")
	}
	if c.SMTP.InboundAddr != next.SMTP.InboundAddr {
		changes = append(changes, "smtp listener")
	}
	if c.IMAP.Addr != next.IMAP.Addr {
		changes = append(changes, "imap listener")
	}
	if c.Submission != next.Submission {
		changes = append(changes, "submission listeners")
	}
	if c.TLS != next.TLS {
		changes = append(changes, "tls")
	}
	if c.Workers.Enabled != next.Workers.Enabled {
		changes = append(changes, "workers.enabled")
	}

	return ReloadImpact{StructuralChanges: changes}
}

func (i ReloadImpact) RequiresRestart() bool {
	return len(i.StructuralChanges) > 0
}

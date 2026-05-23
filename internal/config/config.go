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
	Addr string `pkl:"addr" json:"addr"`
}

type SubmissionConfig struct {
	StartTLSAddr string `pkl:"starttls_addr" json:"starttls_addr"`
	TLSAddr      string `pkl:"tls_addr" json:"tls_addr"`
}

type RelayConfig struct {
	Addr     string `pkl:"addr" json:"addr"`
	Username string `pkl:"username" json:"username"`
	Password string `pkl:"password" json:"password"`
	FromHost string `pkl:"from_host" json:"from_host"`
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
}

type WebhookConfig struct {
	DefaultTimeoutSeconds int               `pkl:"default_timeout_seconds" json:"default_timeout_seconds"`
	Secret                string            `pkl:"secret" json:"secret"`
	Endpoints             map[string]string `pkl:"endpoints" json:"endpoints"`
}

type NotificationConfig struct {
	FromAddress      string `pkl:"from_address" json:"from_address"`
	RenderURL        string `pkl:"render_url" json:"render_url"`
	RenderSecret     string `pkl:"render_secret" json:"render_secret"`
	TimeoutSeconds   int    `pkl:"timeout_seconds" json:"timeout_seconds"`
	MaxResponseBytes int64  `pkl:"max_response_bytes" json:"max_response_bytes"`
}

type SpamConfig struct {
	JunkThreshold   float64 `pkl:"junk_threshold" json:"junk_threshold"`
	RejectThreshold float64 `pkl:"reject_threshold" json:"reject_threshold"`
}

type LimitsConfig struct {
	MaxMessageSizeBytes  int64 `pkl:"max_message_size_bytes" json:"max_message_size_bytes"`
	MaxInboundRecipients int   `pkl:"max_inbound_recipients" json:"max_inbound_recipients"`
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
	if c.Relay != next.Relay {
		changes = append(changes, "relay")
	}
	if c.TLS != next.TLS {
		changes = append(changes, "tls")
	}

	return ReloadImpact{StructuralChanges: changes}
}

func (i ReloadImpact) RequiresRestart() bool {
	return len(i.StructuralChanges) > 0
}

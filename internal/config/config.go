package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/apple/pkl-go/pkl"
)

type Config struct {
	Server     ServerConfig     `pkl:"server"`
	SMTP       SMTPConfig       `pkl:"smtp"`
	IMAP       IMAPConfig       `pkl:"imap"`
	Submission SubmissionConfig `pkl:"submission"`
	Postgres   PostgresConfig   `pkl:"postgres"`
	S3         S3Config         `pkl:"s3"`
	Logging    LoggingConfig    `pkl:"logging"`
	Metrics    MetricsConfig    `pkl:"metrics"`
	API        APIConfig        `pkl:"api"`
	Workers    WorkersConfig    `pkl:"workers"`
	TLS        TLSConfig        `pkl:"tls"`
	Webhooks   WebhookConfig    `pkl:"webhooks"`
	Limits     LimitsConfig     `pkl:"limits"`
}

type ServerConfig struct {
	APIAddr                string `pkl:"api_addr"`
	PublicHostname         string `pkl:"public_hostname"`
	InstanceName           string `pkl:"instance_name"`
	ShutdownTimeoutSeconds int    `pkl:"shutdown_timeout_seconds"`
}

type SMTPConfig struct {
	InboundAddr          string `pkl:"inbound_addr"`
	MaxMessageSizeBytes  int64  `pkl:"max_message_size_bytes"`
	MaxInboundRecipients int    `pkl:"max_inbound_recipients"`
}

type IMAPConfig struct {
	Addr string `pkl:"addr"`
}

type SubmissionConfig struct {
	StartTLSAddr string `pkl:"starttls_addr"`
	TLSAddr      string `pkl:"tls_addr"`
}

func (c ServerConfig) ShutdownTimeout() time.Duration {
	if c.ShutdownTimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.ShutdownTimeoutSeconds) * time.Second
}

type PostgresConfig struct {
	DSN string `pkl:"dsn"`
}

type S3Config struct {
	Endpoint        string `pkl:"endpoint"`
	Region          string `pkl:"region"`
	Bucket          string `pkl:"bucket"`
	AccessKeyID     string `pkl:"access_key_id"`
	SecretAccessKey string `pkl:"secret_access_key"`
	UseSSL          bool   `pkl:"use_ssl"`
}

type LoggingConfig struct {
	Level        string `pkl:"level"`
	AxiomEnabled bool   `pkl:"axiom_enabled"`
	AxiomDataset string `pkl:"axiom_dataset"`
}

type MetricsConfig struct {
	Enabled bool `pkl:"enabled"`
}

type APIConfig struct {
	ServiceToken string `pkl:"service_token"`
}

type WorkersConfig struct {
	Enabled     bool `pkl:"enabled"`
	Concurrency int  `pkl:"concurrency"`
}

type TLSConfig struct {
	Mode          string `pkl:"mode"`
	CertFile      string `pkl:"cert_file"`
	KeyFile       string `pkl:"key_file"`
	ACMEEmail     string `pkl:"acme_email"`
	ACMECA        string `pkl:"acme_ca"`
	ACMECachePath string `pkl:"acme_cache_path"`
}

type WebhookConfig struct {
	DefaultTimeoutSeconds int               `pkl:"default_timeout_seconds"`
	Endpoints             map[string]string `pkl:"endpoints"`
}

type LimitsConfig struct {
	MaxMessageSizeBytes  int64 `pkl:"max_message_size_bytes"`
	MaxInboundRecipients int   `pkl:"max_inbound_recipients"`
}

type ReloadImpact struct {
	StructuralChanges []string
}

func Load(ctx context.Context, path string) (*Config, error) {
	evaluator, err := pkl.NewEvaluator(ctx, pkl.PreconfiguredOptions)
	if err != nil {
		return nil, fmt.Errorf("create pkl evaluator: %w", err)
	}
	defer evaluator.Close()

	var cfg Config
	if err := evaluator.EvaluateModule(ctx, pkl.FileSource(path), &cfg); err != nil {
		return nil, fmt.Errorf("evaluate pkl module: %w", err)
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
	if c.Limits.MaxMessageSizeBytes < 0 {
		problems = append(problems, "limits.max_message_size_bytes must be >= 0")
	}
	if c.Limits.MaxInboundRecipients < 0 {
		problems = append(problems, "limits.max_inbound_recipients must be >= 0")
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

	return ReloadImpact{StructuralChanges: changes}
}

func (i ReloadImpact) RequiresRestart() bool {
	return len(i.StructuralChanges) > 0
}

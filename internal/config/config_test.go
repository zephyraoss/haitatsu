package config

import "testing"

func TestValidateRequiresStructuralFields(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestReloadImpactAllowsReloadableChanges(t *testing.T) {
	base := validConfig()
	next := validConfig()
	next.Logging.Level = "debug"
	next.Workers.Concurrency = 8
	next.Webhooks.DefaultTimeoutSeconds = 20

	impact := base.ReloadImpact(&next)
	if impact.RequiresRestart() {
		t.Fatalf("expected reloadable changes, got %v", impact.StructuralChanges)
	}
}

func TestReloadImpactDetectsStructuralChanges(t *testing.T) {
	base := validConfig()
	next := validConfig()
	next.Database.DSN = "postgres://other"

	impact := base.ReloadImpact(&next)
	if !impact.RequiresRestart() {
		t.Fatal("expected restart requirement")
	}
}

func validConfig() Config {
	return Config{
		Server: ServerConfig{
			APIAddr:        "127.0.0.1:8080",
			PublicHostname: "mail.example.com",
			InstanceName:   "dev-01",
		},
		SMTP:       SMTPConfig{InboundAddr: "127.0.0.1:2525"},
		IMAP:       IMAPConfig{Addr: "127.0.0.1:1143"},
		Submission: SubmissionConfig{StartTLSAddr: "127.0.0.1:1587", TLSAddr: "127.0.0.1:1465"},
		Relay:      RelayConfig{Addr: "smtp.example.com:587"},
		Database:   DatabaseConfig{Driver: "postgres", DSN: "postgres://localhost/haitatsu"},
		S3: S3Config{
			Endpoint:        "localhost:9000",
			Bucket:          "haitatsu",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
		},
	}
}

func TestEffectiveDatabaseSupportsLegacyPostgresConfig(t *testing.T) {
	cfg := Config{Postgres: PostgresConfig{DSN: "postgres://localhost/haitatsu"}}
	got := cfg.EffectiveDatabase()
	if got.Driver != "postgres" || got.DSN != cfg.Postgres.DSN {
		t.Fatalf("effective database = %+v", got)
	}
}

func TestValidateAcceptsSQLiteAndLibSQL(t *testing.T) {
	for _, database := range []DatabaseConfig{
		{Driver: "sqlite", DSN: "file:haitatsu.db"},
		{Driver: "libsql", DSN: "libsql://example.turso.io", AuthToken: "secret", Namespace: "haitatsu"},
	} {
		cfg := validConfig()
		cfg.Database = database
		if err := cfg.Validate(); err != nil {
			t.Errorf("validate %s: %v", database.Driver, err)
		}
	}
}

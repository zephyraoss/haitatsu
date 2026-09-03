package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/zephyraoss/haitatsu/internal/bounce"
	"github.com/zephyraoss/haitatsu/internal/certs"
	"github.com/zephyraoss/haitatsu/internal/cleanup"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/health"
	"github.com/zephyraoss/haitatsu/internal/httpapi"
	"github.com/zephyraoss/haitatsu/internal/imapserver"
	"github.com/zephyraoss/haitatsu/internal/importexport"
	"github.com/zephyraoss/haitatsu/internal/logging"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/notifications"
	"github.com/zephyraoss/haitatsu/internal/outbound"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/rules"
	inboundsmtp "github.com/zephyraoss/haitatsu/internal/smtp/inbound"
	submissionsmtp "github.com/zephyraoss/haitatsu/internal/smtp/submission"
	"github.com/zephyraoss/haitatsu/internal/spam"
	"github.com/zephyraoss/haitatsu/internal/storage"
	"github.com/zephyraoss/haitatsu/internal/version"
	"github.com/zephyraoss/haitatsu/internal/webhooks"
)

type Options struct {
	ConfigPath string
}

type App struct {
	configPath string
	config     *config.Holder
	logger     *slog.Logger
	database   *database.Client
	storage    *storage.Client
	notifier   *mailstore.Notifier
	server     *httpapi.Server
	smtp       *inboundsmtp.Server
	imap       *imapserver.Server
	submission *submissionsmtp.Server
	relay      *outbound.Worker
	webhooks   *webhooks.Worker
	cleanup    *cleanup.Worker
	exports    *importexport.ExportWorker
	imports    *importexport.ImportWorker
}

func New(ctx context.Context, opts Options) (*App, error) {
	if opts.ConfigPath == "" {
		return nil, errors.New("config path is required")
	}

	cfg, err := config.Load(ctx, opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	logger, err := logging.New(cfg.Logging)
	if err != nil {
		return nil, fmt.Errorf("configure logging: %w", err)
	}
	logger = logger.With("version", version.Version, "instance", cfg.Server.InstanceName)
	slog.SetDefault(logger)

	db, err := database.Open(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.RunMigrations(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	blobStore, err := storage.New(cfg.S3)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure storage: %w", err)
	}

	holder := config.NewHolder(cfg)
	notifier := mailstore.NewNotifier(cfg.Server.InstanceName, database.NewChangeBus(db.SQL()))
	mail := mailstore.New(db.Ent(), notifier)
	runtime := &App{configPath: opts.ConfigPath, config: holder, logger: logger, database: db, storage: blobStore, notifier: notifier}
	m := metrics.New()
	checker := health.NewChecker(db, blobStore)
	submissionService := outbound.NewSubmission(db.Ent(), blobStore, mail, cfg.Server.PublicHostname, cfg.Server.InstanceName, func() outbound.Limits {
		limits := holder.Get().Limits
		return outbound.Limits{PerHour: limits.DefaultOutboundPerHour, PerDay: limits.DefaultOutboundPerDay, RecipientsPerMessage: limits.DefaultOutboundRecipients}
	})
	server := httpapi.New(holder, db.Ent(), db.SQL(), blobStore, mail, submissionService, checker, m, runtime)
	cleanupWorker := cleanup.New(db.Ent(), blobStore, mail, m)
	eventService := events.New(db.Ent())
	exportWorker := importexport.NewExportWorker(db.SQL(), db.Ent(), blobStore, eventService, cfg.Server.InstanceName)
	importWorker := importexport.NewImportWorker(db.SQL(), db.Ent(), blobStore, mail, eventService, cfg.Server.InstanceName)
	webhookWorker := webhooks.NewWorker(db.SQL(), db.Ent(), func() config.WebhookConfig { return holder.Get().Webhooks }, m, cfg.Server.InstanceName)
	resolver := routing.NewResolver(db.Ent())
	ruleEngine := rules.New(db.Ent(), mail, eventService)
	messageService := messages.NewService(db.Ent(), blobStore, mail, eventService, ruleEngine, m, cfg.Server.PublicHostname, cfg.Server.InstanceName)
	notificationService := notifications.New(db.Ent(), messageService, func() config.NotificationConfig { return holder.Get().Notifications }, cfg.Server.PublicHostname)
	relayWorker := outbound.NewWorker(db.SQL(), db.Ent(), blobStore, func() config.RelayConfig { return holder.Get().Relay }, m, notificationService, cfg.Server.InstanceName)
	bounceHandler := bounce.NewHandler(db.Ent(), blobStore, m)
	spamChecker := spam.NewChecker(db.Ent(), func() config.SpamConfig { return holder.Get().Spam }, cfg.Server.PublicHostname)
	tlsConfig, err := certs.TLSConfig(ctx, certs.Options{TLS: cfg.TLS, S3: cfg.S3, PublicHostname: cfg.Server.PublicHostname})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure listener tls: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(cfg.TLS.Mode), "acme") {
		logger.Info("acme certificate provisioning started in background", "hostname", cfg.Server.PublicHostname)
	}
	smtpServer := inboundsmtp.New(cfg.SMTP, cfg.Server.PublicHostname, tlsConfig, resolver, messageService, bounceHandler, spamChecker, m, inboundsmtp.Options{
		MaxMessageBytes:     cfg.InboundMessageSize(),
		MaxRecipients:       cfg.InboundRecipients(),
		MaxConnectionsPerIP: cfg.ConnectionsPerIP(),
		MessagesPerMinute:   cfg.InboundMessagesPerMinute(),
	})
	imapServer := imapserver.New(cfg.IMAP, tlsConfig, db.Ent(), blobStore, mail, m, imapserver.Options{
		MaxConnectionsPerIP: cfg.IMAPConnectionsPerIP(),
		AppendLimit:         cfg.InboundMessageSize(),
	})
	submissionServer := submissionsmtp.New(cfg.Submission, cfg.Server.PublicHostname, tlsConfig, db.Ent(), submissionService, submissionsmtp.Options{
		MaxMessageBytes:     cfg.InboundMessageSize(),
		MaxRecipients:       cfg.SubmissionRecipients(),
		MaxConnectionsPerIP: cfg.ConnectionsPerIP(),
	})

	runtime.server = server
	runtime.smtp = smtpServer
	runtime.imap = imapServer
	runtime.submission = submissionServer
	runtime.relay = relayWorker
	runtime.webhooks = webhookWorker
	runtime.cleanup = cleanupWorker
	runtime.exports = exportWorker
	runtime.imports = importWorker
	return runtime, nil
}

func (a *App) Run(ctx context.Context) error {
	cfg := a.config.Get()
	a.notifier.Start(ctx)
	errCh := make(chan error, 4)
	go func() {
		a.logger.Info("starting http api", "addr", cfg.Server.APIAddr)
		errCh <- a.server.Listen()
	}()
	go func() {
		a.logger.Info("starting smtp inbound", "addr", cfg.SMTP.InboundAddr)
		errCh <- a.smtp.Listen()
	}()
	go func() {
		a.logger.Info("starting imap", "addr", cfg.IMAP.Addr)
		errCh <- a.imap.Listen()
	}()
	go func() {
		a.logger.Info("starting smtp submission", "starttls_addr", cfg.Submission.StartTLSAddr, "tls_addr", cfg.Submission.TLSAddr)
		errCh <- a.submission.Listen()
	}()
	if cfg.Workers.Enabled {
		a.logger.Info("starting outbound relay workers", "concurrency", cfg.Workers.Concurrency)
		a.relay.Run(ctx, cfg.Workers.Concurrency)
		a.logger.Info("starting webhook workers", "concurrency", cfg.Workers.Concurrency)
		a.webhooks.Run(ctx, cfg.Workers.Concurrency)
		a.logger.Info("starting cleanup worker")
		a.cleanup.Run(ctx)
		a.logger.Info("starting import export workers", "concurrency", cfg.Workers.Concurrency)
		a.exports.Run(ctx, cfg.Workers.Concurrency)
		a.imports.Run(ctx, cfg.Workers.Concurrency)
	}

	select {
	case <-ctx.Done():
		if err := a.shutdown(); err != nil {
			return err
		}
		return ctx.Err()
	case err := <-errCh:
		_ = a.shutdown()
		return err
	}
}

func (a *App) shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.config.Get().Server.ShutdownTimeout())
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown http server: %w", err)
	}
	if err := a.smtp.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown smtp server: %w", err)
	}
	if err := a.imap.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown imap server: %w", err)
	}
	if err := a.submission.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown submission server: %w", err)
	}
	return nil
}

func (a *App) NotifyReload(signals ...os.Signal) {
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, signals...)

	go func() {
		for range signalCh {
			if err := a.Reload(context.Background()); err != nil {
				a.logger.Error("config reload failed", "error", err)
			}
		}
	}()
}

func (a *App) Reload(ctx context.Context) error {
	next, err := config.Load(ctx, a.configPath)
	if err != nil {
		return fmt.Errorf("load reload config: %w", err)
	}

	current := a.config.Get()
	impact := current.ReloadImpact(next)
	if impact.RequiresRestart() {
		return fmt.Errorf("reload requires restart: %v", impact.StructuralChanges)
	}

	logger, err := logging.New(next.Logging)
	if err != nil {
		return fmt.Errorf("configure reloaded logging: %w", err)
	}

	a.config.Set(next)
	a.logger = logger
	slog.SetDefault(logger)
	if next.Workers.Concurrency != current.Workers.Concurrency {
		a.logger.Warn("workers.concurrency changed; new value applies after restart", "current", current.Workers.Concurrency, "next", next.Workers.Concurrency)
	}
	a.logger.Info("config reloaded")
	return nil
}

func (a *App) Close() {
	if a == nil {
		return
	}
	if a.database != nil {
		a.database.Close()
	}
}

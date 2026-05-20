package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

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
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/outbound"
	"github.com/zephyraoss/haitatsu/internal/routing"
	inboundsmtp "github.com/zephyraoss/haitatsu/internal/smtp/inbound"
	submissionsmtp "github.com/zephyraoss/haitatsu/internal/smtp/submission"
	"github.com/zephyraoss/haitatsu/internal/spam"
	"github.com/zephyraoss/haitatsu/internal/storage"
	"github.com/zephyraoss/haitatsu/internal/webhooks"
)

type Options struct {
	ConfigPath string
}

type App struct {
	configPath string
	config     *config.Config
	logger     *slog.Logger
	database   *database.Client
	storage    *storage.Client
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

	runtime := &App{configPath: opts.ConfigPath, config: cfg, logger: logger, database: db, storage: blobStore}
	m := metrics.New()
	checker := health.NewChecker(db, blobStore)
	submissionService := outbound.NewSubmission(db.Ent(), blobStore, cfg.Server.PublicHostname, cfg.Server.InstanceName, cfg.Bounce.Domain)
	server := httpapi.New(cfg, db.Ent(), db.SQL(), blobStore, submissionService, checker, m, runtime)
	relayWorker := outbound.NewWorker(db.SQL(), db.Ent(), blobStore, cfg.Relay, cfg.Server.InstanceName)
	cleanupWorker := cleanup.New(db.Ent())
	eventService := events.New(db.Ent())
	exportWorker := importexport.NewExportWorker(db.SQL(), db.Ent(), blobStore, eventService, cfg.Server.InstanceName)
	importWorker := importexport.NewImportWorker(db.SQL(), db.Ent(), eventService, cfg.Server.InstanceName)
	webhookWorker := webhooks.NewWorker(db.SQL(), db.Ent(), cfg.Webhooks, cfg.Server.InstanceName)
	resolver := routing.NewResolver(db.Ent())
	messageService := messages.NewService(db.Ent(), blobStore, eventService, cfg.Server.PublicHostname, cfg.Server.InstanceName)
	bounceHandler := bounce.NewHandler(db.Ent(), blobStore, cfg.Bounce.Domain)
	spamChecker := spam.NewChecker(db.Ent(), cfg.Spam, cfg.Server.PublicHostname)
	tlsConfig, err := certs.TLSConfig(ctx, cfg.TLS, cfg.Server.PublicHostname)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure listener tls: %w", err)
	}
	smtpServer := inboundsmtp.New(cfg.SMTP, cfg.Server.PublicHostname, tlsConfig, resolver, messageService, bounceHandler, spamChecker)
	imapServer := imapserver.New(cfg.IMAP, tlsConfig, db.Ent(), blobStore)
	submissionServer := submissionsmtp.New(cfg.Submission, cfg.Server.PublicHostname, tlsConfig, db.Ent(), submissionService)

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
	errCh := make(chan error, 4)
	go func() {
		a.logger.Info("starting http api", "addr", a.config.Server.APIAddr)
		errCh <- a.server.Listen()
	}()
	go func() {
		a.logger.Info("starting smtp inbound", "addr", a.config.SMTP.InboundAddr)
		errCh <- a.smtp.Listen()
	}()
	go func() {
		a.logger.Info("starting imap", "addr", a.config.IMAP.Addr)
		errCh <- a.imap.Listen()
	}()
	go func() {
		a.logger.Info("starting smtp submission", "starttls_addr", a.config.Submission.StartTLSAddr, "tls_addr", a.config.Submission.TLSAddr)
		errCh <- a.submission.Listen()
	}()
	if a.config.Workers.Enabled {
		a.logger.Info("starting outbound relay workers", "concurrency", a.config.Workers.Concurrency)
		a.relay.Run(ctx, a.config.Workers.Concurrency)
		a.logger.Info("starting webhook workers", "concurrency", a.config.Workers.Concurrency)
		a.webhooks.Run(ctx, a.config.Workers.Concurrency)
		a.logger.Info("starting cleanup worker")
		a.cleanup.Run(ctx)
		a.logger.Info("starting import export workers", "concurrency", a.config.Workers.Concurrency)
		a.exports.Run(ctx, a.config.Workers.Concurrency)
		a.imports.Run(ctx, a.config.Workers.Concurrency)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.config.Server.ShutdownTimeout())
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

	impact := a.config.ReloadImpact(next)
	if impact.RequiresRestart() {
		return fmt.Errorf("reload requires restart: %v", impact.StructuralChanges)
	}

	logger, err := logging.New(next.Logging)
	if err != nil {
		return fmt.Errorf("configure reloaded logging: %w", err)
	}

	a.config = next
	a.logger = logger
	slog.SetDefault(logger)
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

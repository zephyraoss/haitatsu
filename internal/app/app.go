package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/health"
	"github.com/zephyraoss/haitatsu/internal/httpapi"
	"github.com/zephyraoss/haitatsu/internal/logging"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	inboundsmtp "github.com/zephyraoss/haitatsu/internal/smtp/inbound"
	"github.com/zephyraoss/haitatsu/internal/storage"
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

	m := metrics.New()
	checker := health.NewChecker(db, blobStore)
	server := httpapi.New(cfg.Server, cfg.API, db.Ent(), blobStore, checker, m)
	resolver := routing.NewResolver(db.Ent())
	messageService := messages.NewService(db.Ent(), blobStore, cfg.Server.PublicHostname, cfg.Server.InstanceName)
	tlsConfig, err := smtpTLSConfig(cfg.TLS)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure smtp tls: %w", err)
	}
	smtpServer := inboundsmtp.New(cfg.SMTP, cfg.Server.PublicHostname, tlsConfig, resolver, messageService)

	return &App{
		configPath: opts.ConfigPath,
		config:     cfg,
		logger:     logger,
		database:   db,
		storage:    blobStore,
		server:     server,
		smtp:       smtpServer,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	go func() {
		a.logger.Info("starting http api", "addr", a.config.Server.APIAddr)
		errCh <- a.server.Listen()
	}()
	go func() {
		a.logger.Info("starting smtp inbound", "addr", a.config.SMTP.InboundAddr)
		errCh <- a.smtp.Listen()
	}()

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

func smtpTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	if cfg.CertFile == "" && cfg.KeyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

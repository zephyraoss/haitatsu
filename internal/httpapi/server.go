package httpapi

import (
	"context"
	"database/sql"
	"net"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/zephyraoss/haitatsu/internal/api"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/health"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/outbound"
	"github.com/zephyraoss/haitatsu/internal/version"
)

type Server struct {
	app  *fiber.App
	addr string
}

type MessageStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
}

type Reloader interface {
	Reload(ctx context.Context) error
}

func New(cfg *config.Holder, entClient *ent.Client, db *sql.DB, store MessageStore, mail *mailstore.Store, submission *outbound.Submission, checker *health.Checker, m *metrics.Metrics, reloader Reloader) *Server {
	app := fiber.New(fiber.Config{AppName: "Haitatsu", BodyLimit: 64 * 1024 * 1024})
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Haitatsu-Version", version.Version)
		return c.Next()
	})
	app.Use(m.Middleware)

	app.Get("/health", func(c fiber.Ctx) error {
		if err := checker.Health(c.Context()); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "unhealthy", "error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/ready", func(c fiber.Ctx) error {
		if err := checker.Ready(c.Context()); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "not_ready", "error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ready"})
	})

	token := func() string { return cfg.Get().API.ServiceToken }
	app.Get("/metrics", api.ServiceTokenMiddleware(token), adaptor.HTTPHandler(m.Handler()))
	api.Register(app, entClient, db, store, mail, submission, cfg, reloader)

	return &Server{app: app, addr: cfg.Get().Server.APIAddr}
}

func (s *Server) Listen() error {
	return s.app.Listen(s.addr, fiber.ListenConfig{DisableStartupMessage: true})
}

func (s *Server) Serve(listener net.Listener) error {
	return s.app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
}

func (s *Server) Handler() *fiber.App {
	return s.app
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

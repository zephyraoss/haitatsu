package httpapi

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/zephyraoss/haitatsu/internal/api"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/health"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/outbound"
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

func New(cfg *config.Config, entClient *ent.Client, store MessageStore, submission *outbound.Submission, checker *health.Checker, m *metrics.Metrics, reloader Reloader) *Server {
	app := fiber.New(fiber.Config{AppName: "Haitatsu"})
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

	app.Get("/metrics", adaptor.HTTPHandler(m.Handler()))
	api.Register(app, entClient, store, submission, *cfg, reloader)

	return &Server{app: app, addr: cfg.Server.APIAddr}
}

func (s *Server) Listen() error {
	return s.app.Listen(s.addr)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

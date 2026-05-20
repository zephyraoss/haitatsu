package httpapi

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/health"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

type Server struct {
	app  *fiber.App
	addr string
}

func New(cfg config.ServerConfig, checker *health.Checker, m *metrics.Metrics) *Server {
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

	return &Server{app: app, addr: cfg.APIAddr}
}

func (s *Server) Listen() error {
	return s.app.Listen(s.addr)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

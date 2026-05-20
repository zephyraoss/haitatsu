package api

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func ServiceTokenMiddleware(cfg config.APIConfig) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !hasServiceToken(c.Get("Authorization"), cfg.ServiceToken) {
			return problem(c, fiber.StatusUnauthorized, "unauthorized", "Authentication required")
		}
		return c.Next()
	}
}

func hasServiceToken(header string, serviceToken string) bool {
	if serviceToken == "" || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	return strings.TrimPrefix(header, "Bearer ") == serviceToken
}

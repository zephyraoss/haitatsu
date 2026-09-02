package api

import (
	"crypto/subtle"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/config"
)

type TokenSource func() string

func ServiceTokenMiddleware(token TokenSource) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !hasServiceToken(c.Get("Authorization"), token()) {
			return problem(c, fiber.StatusUnauthorized, "unauthorized", "Authentication required")
		}
		return c.Next()
	}
}

func StaticToken(cfg config.APIConfig) TokenSource {
	return func() string { return cfg.ServiceToken }
}

func hasServiceToken(header string, serviceToken string) bool {
	if serviceToken == "" || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimPrefix(header, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(presented), []byte(serviceToken)) == 1
}

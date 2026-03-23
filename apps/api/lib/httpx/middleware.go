package httpx

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

func RequestLogger(log *zap.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()

		log.Info("request",
			zap.String("method", c.Method()),
			zap.String("path", c.Path()),
			zap.Int("status", c.Response().StatusCode()),
			zap.String("trace_id", c.Get("X-Request-ID")),
		)

		return err
	}
}

func AuthMiddleware(clerkSecretKey string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		auth := c.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "missing authorization token"})
		}

		// TODO: validate Clerk JWT and extract claims
		// For now, extract store_id from a custom header for development
		storeID := c.Get("X-Store-ID")
		if storeID == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "missing store context"})
		}

		c.Locals("store_id", storeID)
		return c.Next()
	}
}

func SubscriptionMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// TODO: check subscription status from DB
		// For now, pass through
		return c.Next()
	}
}

package httpx

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"livecart/apps/api/lib/clerk"
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

func AuthMiddleware(clerkClient *clerk.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		auth := c.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "missing authorization token"})
		}

		token := strings.TrimPrefix(auth, "Bearer ")

		// Validate Clerk JWT
		claims, err := clerkClient.ValidateToken(c.Context(), token)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "invalid token: " + err.Error()})
		}

		// Set user info in context
		c.Locals("user_id", claims.UserID)
		c.Locals("email", claims.Email)
		c.Locals("claims", claims)

		// Extract store_id from public metadata if available
		if storeID, ok := claims.Metadata["store_id"].(string); ok && storeID != "" {
			c.Locals("store_id", storeID)
		}

		return c.Next()
	}
}

// RequireStore middleware ensures a store_id is present in context
// Should be used after AuthMiddleware for routes that require store context
func RequireStore() fiber.Handler {
	return func(c *fiber.Ctx) error {
		storeID := c.Locals("store_id")
		if storeID == nil || storeID == "" {
			return c.Status(fiber.StatusForbidden).JSON(Envelope{Error: "no store associated with this user"})
		}
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

// StoreAccessValidator validates if a user has access to a store
type StoreAccessValidator interface {
	ValidateStoreAccess(ctx context.Context, clerkUserID, storeID string) (bool, error)
}

// StoreAccessMiddleware validates that the authenticated user has access to the store in the URL
func StoreAccessMiddleware(validator StoreAccessValidator) fiber.Handler {
	return func(c *fiber.Ctx) error {
		storeID := c.Params("storeId")
		if storeID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(Envelope{Error: "store_id is required"})
		}

		userID := GetUserID(c)
		if userID == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "unauthorized"})
		}

		hasAccess, err := validator.ValidateStoreAccess(c.Context(), userID, storeID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(Envelope{Error: "failed to validate store access"})
		}

		if !hasAccess {
			return c.Status(fiber.StatusForbidden).JSON(Envelope{Error: "you don't have access to this store"})
		}

		// Set store_id in context for handlers
		c.Locals("store_id", storeID)
		return c.Next()
	}
}

// Helper functions to get values from context
func GetUserID(c *fiber.Ctx) string {
	if v := c.Locals("user_id"); v != nil {
		return v.(string)
	}
	return ""
}

func GetStoreID(c *fiber.Ctx) string {
	if v := c.Locals("store_id"); v != nil {
		return v.(string)
	}
	return ""
}

func GetEmail(c *fiber.Ctx) string {
	if v := c.Locals("email"); v != nil {
		return v.(string)
	}
	return ""
}

func GetClaims(c *fiber.Ctx) *clerk.Claims {
	if v := c.Locals("claims"); v != nil {
		return v.(*clerk.Claims)
	}
	return nil
}

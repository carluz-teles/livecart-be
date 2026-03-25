package httpx

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"livecart/apps/api/lib/clerk"
	"livecart/apps/api/lib/config"
)

func RequestLogger(log *zap.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()

		// Build log fields - add user_id and store_id if available
		fields := []zap.Field{
			zap.String("method", c.Method()),
			zap.String("path", c.Path()),
			zap.Int("status", c.Response().StatusCode()),
			zap.String("trace_id", c.Get("X-Request-ID")),
		}

		if userID := GetUserID(c); userID != "" {
			fields = append(fields, zap.String("user_id", userID))
		}

		if storeID := GetStoreID(c); storeID != "" {
			fields = append(fields, zap.String("store_id", storeID))
		}

		log.Info("request", fields...)

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

		// Dev bypass: use X-Dev-User-ID header in development
		if !config.IsProduction() && c.Get("X-Dev-User-ID") != "" {
			c.Locals("user_id", c.Get("X-Dev-User-ID"))
			c.Locals("email", "dev@test.com")
			return c.Next()
		}

		// Validate Clerk JWT
		claims, err := clerkClient.ValidateToken(c.Context(), token)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "invalid token: " + err.Error()})
		}

		// Set user info in context
		c.Locals("user_id", claims.UserID)
		c.Locals("email", claims.Email)
		c.Locals("claims", claims)

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
	// GetStoreAccessInfo returns membershipID, role, userID (internal UUID), error
	GetStoreAccessInfo(ctx context.Context, clerkUserID, storeID string) (membershipID string, role string, userID string, err error)
}

// StoreAccessMiddleware validates that the authenticated user has access to the store in the URL
func StoreAccessMiddleware(validator StoreAccessValidator) fiber.Handler {
	return func(c *fiber.Ctx) error {
		storeID := c.Params("storeId")
		if storeID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(Envelope{Error: "store_id is required"})
		}

		clerkUserID := GetUserID(c) // This is Clerk user ID from JWT
		if clerkUserID == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: "unauthorized"})
		}

		membershipID, role, internalUserID, err := validator.GetStoreAccessInfo(c.Context(), clerkUserID, storeID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(Envelope{Error: "failed to validate store access"})
		}

		if membershipID == "" {
			return c.Status(fiber.StatusForbidden).JSON(Envelope{Error: "you don't have access to this store"})
		}

		// Set store info in context for handlers
		c.Locals("store_id", storeID)
		c.Locals("store_user_id", membershipID)     // Membership ID for this store
		c.Locals("store_role", role)                 // Role in this store
		c.Locals("internal_user_id", internalUserID) // Internal user UUID
		return c.Next()
	}
}

// GetStoreUserID returns the membership ID from context
func GetStoreUserID(c *fiber.Ctx) string {
	if v := c.Locals("store_user_id"); v != nil {
		return v.(string)
	}
	return ""
}

// GetInternalUserID returns the internal user UUID from context
func GetInternalUserID(c *fiber.Ctx) string {
	if v := c.Locals("internal_user_id"); v != nil {
		return v.(string)
	}
	return ""
}

// GetStoreRole returns the user's role in the store from context
func GetStoreRole(c *fiber.Ctx) string {
	if v := c.Locals("store_role"); v != nil {
		return v.(string)
	}
	return ""
}

// RequireRole middleware checks if the user has one of the required roles
func RequireRole(roles ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userRole := GetStoreRole(c)
		if userRole == "" {
			return c.Status(fiber.StatusForbidden).JSON(Envelope{Error: "no role assigned"})
		}

		for _, role := range roles {
			if userRole == role {
				return c.Next()
			}
		}

		return c.Status(fiber.StatusForbidden).JSON(Envelope{Error: "insufficient permissions"})
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

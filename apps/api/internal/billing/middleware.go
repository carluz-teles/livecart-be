package billing

import (
	"context"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

// AccessGuard blocks store-scoped requests for stores whose subscription is
// blocked (PRD 007 §5). Billing endpoints stay reachable so the merchant can
// pay their way back in.
//
// Posture is fail-open: a billing lookup error must never take a paying
// merchant down — enforcement resumes on the next healthy request.
func (s *Service) AccessGuard() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// The merchant needs these to convert/regularize.
		if strings.Contains(c.Path(), "/billing") {
			return c.Next()
		}

		storeID, _ := c.Locals("store_id").(string)
		if storeID == "" {
			return c.Next()
		}

		state, err := s.GetState(c.Context(), storeID)
		if err != nil {
			s.logger.Warn("billing guard lookup failed (fail-open)",
				zap.String("store_id", storeID),
				zap.Error(err),
			)
			return c.Next()
		}
		if state == nil {
			// Legacy store without a subscription row — /users/sync lazily
			// creates the trial; don't lock the account out meanwhile.
			return c.Next()
		}

		if state.Blocked {
			return c.Status(http.StatusPaymentRequired).JSON(fiber.Map{
				"error":        "subscription_required",
				"message":      "Sua assinatura está inativa. Escolha um plano para continuar.",
				"subscription": state,
			})
		}
		return c.Next()
	}
}

// IsStoreBlocked is the narrow gate used by workers and the comment pipeline
// (fail-open on errors, consistent with the HTTP guard).
func (s *Service) IsStoreBlocked(ctx context.Context, storeID string) bool {
	state, err := s.GetState(ctx, storeID)
	if err != nil || state == nil {
		return false
	}
	return state.Blocked
}

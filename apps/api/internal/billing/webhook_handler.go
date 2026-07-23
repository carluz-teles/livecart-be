package billing

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// WebhookHandler receives Stripe events.
type WebhookHandler struct {
	service *Service
	logger  *zap.Logger
}

// NewWebhookHandler builds the handler.
func NewWebhookHandler(service *Service, logger *zap.Logger) *WebhookHandler {
	return &WebhookHandler{service: service, logger: logger}
}

// RegisterRoutes mounts the unauthenticated webhook route (signature-guarded).
func (h *WebhookHandler) RegisterRoutes(app *fiber.App) {
	app.Post("/api/webhooks/stripe", h.HandleStripe)
}

// HandleStripe verifies the signature and applies the event.
// @Summary Handle Stripe webhook
// @Description Applies subscription lifecycle events (signature-verified)
// @Tags webhooks
// @Accept json
// @Produce json
// @Success 200 {object} map[string]string
// @Router /api/webhooks/stripe [post]
func (h *WebhookHandler) HandleStripe(c *fiber.Ctx) error {
	payload := c.Body()

	secret := config.StripeWebhookSecret.String()
	if secret == "" {
		logger.From(c.Context(), h.logger).Warn("stripe webhook received but STRIPE_WEBHOOK_SECRET is not set")
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "webhook not configured"})
	}
	if !VerifyWebhookSignature(payload, c.Get("Stripe-Signature"), secret, time.Now()) {
		logger.From(c.Context(), h.logger).Warn("rejected stripe webhook with invalid signature")
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid signature"})
	}

	var event StripeEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return httpx.ErrBadRequest("invalid event payload")
	}

	// L1 dispatch: emit the subscription.process command to the durable outbox and
	// ACK immediately. The command consumer runs the guarded ProcessWebhookEvent
	// with asynq retry + DLQ, so the handler no longer blocks the request on the
	// DB write. 500 (Stripe retries) only when the emit itself fails.
	if err := h.service.DispatchWebhookEvent(c.UserContext(), &event); err != nil {
		logger.From(c.UserContext(), h.logger).Error("failed to dispatch stripe event",
			zap.String("event", event.ID),
			zap.String("type", event.Type),
			zap.Error(err),
		)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "dispatch failed"})
	}

	return httpx.OK(c, fiber.Map{"received": true})
}

package billing

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
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
		h.logger.Warn("stripe webhook received but STRIPE_WEBHOOK_SECRET is not set")
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "webhook not configured"})
	}
	if !VerifyWebhookSignature(payload, c.Get("Stripe-Signature"), secret, time.Now()) {
		h.logger.Warn("rejected stripe webhook with invalid signature")
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid signature"})
	}

	var event StripeEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return httpx.BadRequest(c, "invalid event payload")
	}

	if err := h.service.ProcessWebhookEvent(c.Context(), &event); err != nil {
		// 500 makes Stripe retry — desired for transient DB failures.
		h.logger.Error("failed to process stripe event",
			zap.String("event", event.ID),
			zap.String("type", event.Type),
			zap.Error(err),
		)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "processing failed"})
	}

	return httpx.OK(c, fiber.Map{"received": true})
}

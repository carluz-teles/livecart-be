package integration

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// PAYMENT ADMIN HANDLERS — provider-specific connect endpoints
// =============================================================================
//
// Mercado Pago goes through the OAuth flow in handler.go; Pagar.me uses
// static API keys (no OAuth, no callback) and gets a dedicated endpoint
// that validates the keys live before persisting. Future API-key-based
// payment providers should follow the same shape.

// ConnectPagarmeRequest is the body for POST /payment/pagarme/connect.
type ConnectPagarmeRequest struct {
	SecretKey       string `json:"secretKey" validate:"required,min=10"`
	PublicKey       string `json:"publicKey" validate:"required,min=10"`
	WebhookUsername string `json:"webhookUsername,omitempty"`
	WebhookPassword string `json:"webhookPassword,omitempty"`
}

// ConnectPagarme validates the supplied secret + public against Pagar.me
// and persists the integration as active. Rotation reuses the same endpoint
// — POST again with new keys and the existing row is updated in place.
//
// @Summary Connect Pagar.me
// @Description Validates Pagar.me API keys and activates the payment integration for the store
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body ConnectPagarmeRequest true "Pagar.me connection payload"
// @Success 200 {object} httpx.Envelope{data=IntegrationResponse}
// @Failure 400 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/payment/pagarme/connect [post]
// @Security BearerAuth
func (h *Handler) ConnectPagarme(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req ConnectPagarmeRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.ConnectPagarme(c.Context(), ConnectPagarmeInput{
		StoreID:         storeID,
		SecretKey:       req.SecretKey,
		PublicKey:       req.PublicKey,
		WebhookUsername: req.WebhookUsername,
		WebhookPassword: req.WebhookPassword,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, toIntegrationResponse(output))
}

// PagarmeWebhookStatusResponse is the API shape returned by
// GET /integrations/{id}/pagarme/webhook-status. Empty time fields are
// serialized as null so the UI can branch on "never delivered yet".
type PagarmeWebhookStatusResponse struct {
	ExpectedURL        string     `json:"expectedUrl"`
	Configured         bool       `json:"configured"`
	MatchCount         int        `json:"matchCount"`
	LastDeliveryAt     *time.Time `json:"lastDeliveryAt"`
	LastDeliveryStatus string     `json:"lastDeliveryStatus"`
	LastResponseStatus int        `json:"lastResponseStatus"`
	LastEvent          string     `json:"lastEvent"`
}

// GetPagarmeWebhookStatus checks Pagar.me's delivery history for hooks
// targeting our webhook URL. Useful for confirming the merchant set up
// the webhook in the Pagar.me dashboard without waiting for a real event.
//
// @Summary Pagar.me webhook status
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=PagarmeWebhookStatusResponse}
// @Router /api/v1/stores/{storeId}/integrations/{id}/pagarme/webhook-status [get]
// @Security BearerAuth
func (h *Handler) GetPagarmeWebhookStatus(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")

	output, err := h.service.GetPagarmeWebhookStatus(c.Context(), integrationID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	resp := PagarmeWebhookStatusResponse{
		ExpectedURL:        output.ExpectedURL,
		Configured:         output.Configured,
		MatchCount:         output.MatchCount,
		LastDeliveryStatus: output.LastDeliveryStatus,
		LastResponseStatus: output.LastResponseStatus,
		LastEvent:          output.LastEvent,
	}
	if !output.LastDeliveryAt.IsZero() {
		t := output.LastDeliveryAt
		resp.LastDeliveryAt = &t
	}
	return httpx.OK(c, resp)
}

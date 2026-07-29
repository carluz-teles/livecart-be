package payment

import (
	"context"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// PAYMENT ADMIN HANDLERS — provider-specific connect endpoints
// =============================================================================
//
// This is the extracted admin surface of the payment domain (Bloco B1c): the
// HTTP handler moved verbatim out of internal/integration/payment_handler.go.
//
// Mercado Pago goes through the OAuth flow in integration's handler.go; Pagar.me
// uses static API keys (no OAuth, no callback) and gets a dedicated endpoint
// that validates the keys live before persisting. Future API-key-based payment
// providers should follow the same shape.
//
// The underlying use cases still live in integration.Service; they are reached
// through PagarmeAdminService, declared here (consumer side) and implemented by
// an adapter over *integration.Service. integration already imports payment, so
// the adapter lives there and this package never imports integration — the
// dependency graph stays acyclic. ConnectPagarme returns the already-rendered
// response as `any` so the shared integration.toIntegrationResponse mapper is
// reused (not duplicated) across the package boundary.

// PagarmeAdminService is the slice of the still-integration-owned Pagar.me admin
// use cases that this handler drives. Its contract speaks only in payment-owned
// or primitive types so the payment package stays free of integration imports.
type PagarmeAdminService interface {
	// ConnectPagarme validates the keys and persists the integration, returning
	// the rendered integration response (kept as `any` to avoid importing the
	// integration response type).
	ConnectPagarme(ctx context.Context, in ConnectPagarmeInput) (any, error)
	// GetPagarmeWebhookStatus reports whether Pagar.me has been delivering to
	// our webhook URL, inferred from recent delivery history.
	GetPagarmeWebhookStatus(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookStatusResponse, error)
	// TestPagarmeWebhook runs the loopback self-test against our own public URL.
	TestPagarmeWebhook(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookTestResponse, error)
	// RunPagarmeWebhookLiveTest runs the real end-to-end delivery test.
	RunPagarmeWebhookLiveTest(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookLiveTestResponse, error)
}

// Handler exposes the payment admin endpoints (Pagar.me connect + webhook
// diagnostics) over HTTP.
type Handler struct {
	admin PagarmeAdminService
}

// NewHandler builds the payment admin Handler.
func NewHandler(admin PagarmeAdminService) *Handler {
	return &Handler{admin: admin}
}

// RegisterRoutes mounts the payment admin routes under the shared
// /integrations group. Paths and verbs are unchanged from when they lived in
// integration.Handler.RegisterRoutes — connect is registered before the /:id
// diagnostics so the static segment is matched first.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/integrations")

	// Payment — provider-specific connect (no OAuth). Pagar.me uses static
	// API keys (sk_*/pk_*), so the merchant pastes them into a form and we
	// validate live against the gateway before persisting.
	g.Post("/payment/pagarme/connect", h.ConnectPagarme)
	g.Get("/:id/pagarme/webhook-status", h.GetPagarmeWebhookStatus)
	g.Post("/:id/pagarme/webhook-test", h.TestPagarmeWebhook)
	g.Post("/:id/pagarme/webhook-live-test", h.RunPagarmeWebhookLiveTest)
}

// ConnectPagarmeInput is the service input to set up or rotate the Pagar.me
// payment integration for a store.
type ConnectPagarmeInput struct {
	StoreID         string
	SecretKey       string
	PublicKey       string
	WebhookUsername string
	WebhookPassword string
}

// ConnectPagarmeRequest is the body for POST /payment/pagarme/connect.
type ConnectPagarmeRequest struct {
	SecretKey       string `json:"secretKey"`
	PublicKey       string `json:"publicKey"`
	WebhookUsername string `json:"webhookUsername,omitempty"`
	WebhookPassword string `json:"webhookPassword,omitempty"`
}

// Validate is the syntactic gate (ozzo). It mirrors the former go-playground
// tags (required + min length 10) so the key format is sanity-checked before we
// spend a live TestConnection probe against Pagar.me. The webhook Basic Auth
// pair is optional.
func (r ConnectPagarmeRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.SecretKey, validation.Required, validation.Length(10, 0)),
		validation.Field(&r.PublicKey, validation.Required, validation.Length(10, 0)),
	)
}

// ConnectPagarme validates the supplied secret + public against Pagar.me and
// persists the integration as active. Rotation reuses the same endpoint — POST
// again with new keys and the existing row is updated in place.
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
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	resp, err := h.admin.ConnectPagarme(c.Context(), ConnectPagarmeInput{
		StoreID:         storeID,
		SecretKey:       req.SecretKey,
		PublicKey:       req.PublicKey,
		WebhookUsername: req.WebhookUsername,
		WebhookPassword: req.WebhookPassword,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
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
// targeting our webhook URL. Useful for confirming the merchant set up the
// webhook in the Pagar.me dashboard without waiting for a real event.
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

	resp, err := h.admin.GetPagarmeWebhookStatus(c.Context(), integrationID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
}

// PagarmeWebhookTestResponse is the API shape returned by
// POST /integrations/{id}/pagarme/webhook-test — the loopback self-test.
type PagarmeWebhookTestResponse struct {
	URL            string `json:"url"`
	Reachable      bool   `json:"reachable"`
	Healthy        bool   `json:"healthy"`
	HTTPStatus     int    `json:"httpStatus"`
	AuthConfigured bool   `json:"authConfigured"`
	LatencyMs      int64  `json:"latencyMs"`
	Message        string `json:"message"`
}

// TestPagarmeWebhook runs the loopback self-test: it POSTs a synthetic event to
// the store's own public webhook URL and reports whether our endpoint is
// reachable and healthy. Lets the merchant validate the webhook right after
// configuring it, without waiting for a real customer payment.
//
// @Summary Pagar.me webhook self-test
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=PagarmeWebhookTestResponse}
// @Router /api/v1/stores/{storeId}/integrations/{id}/pagarme/webhook-test [post]
// @Security BearerAuth
func (h *Handler) TestPagarmeWebhook(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")

	resp, err := h.admin.TestPagarmeWebhook(c.Context(), integrationID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
}

// PagarmeWebhookLiveTestResponse is the API shape returned by
// POST /integrations/{id}/pagarme/webhook-live-test — the real round-trip test.
type PagarmeWebhookLiveTestResponse struct {
	ExpectedURL  string `json:"expectedUrl"`
	OrderCode    string `json:"orderCode"`
	Delivered    bool   `json:"delivered"`
	Healthy      bool   `json:"healthy"`
	HTTPStatus   int    `json:"httpStatus"`
	Event        string `json:"event"`
	DeliveredURL string `json:"deliveredUrl"`
	ResponseRaw  string `json:"responseRaw"`
	Message      string `json:"message"`
}

// RunPagarmeWebhookLiveTest runs the REAL end-to-end webhook test: it creates a
// throwaway PIX order so Pagar.me fires a real order.created webhook to the
// merchant's endpoint, confirms delivery via Pagar.me's history, then cancels
// the order. Validates the actual delivery path without waiting for a sale.
//
// @Summary Pagar.me real webhook test
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=PagarmeWebhookLiveTestResponse}
// @Router /api/v1/stores/{storeId}/integrations/{id}/pagarme/webhook-live-test [post]
// @Security BearerAuth
func (h *Handler) RunPagarmeWebhookLiveTest(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")

	resp, err := h.admin.RunPagarmeWebhookLiveTest(c.Context(), integrationID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
}

package integration

import (
	"context"
	"encoding/json"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

// WebhookHandler handles incoming webhooks from external providers.
type WebhookHandler struct {
	service *Service
	logger  *zap.Logger
}

// NewWebhookHandler creates a new webhook handler.
func NewWebhookHandler(service *Service, logger *zap.Logger) *WebhookHandler {
	return &WebhookHandler{
		service: service,
		logger:  logger,
	}
}

// RegisterRoutes registers webhook routes.
// These routes are unauthenticated but use signature verification.
func (h *WebhookHandler) RegisterRoutes(app *fiber.App) {
	webhooks := app.Group("/api/webhooks/integrations")

	// OAuth callbacks
	webhooks.Get("/mercado_pago/oauth/callback", h.HandleMercadoPagoOAuthCallback)
	webhooks.Get("/tiny/oauth/callback", h.HandleTinyOAuthCallback)

	// Payment/event webhooks
	webhooks.Post("/mercado_pago/:integrationId", h.HandleMercadoPago)
	webhooks.Post("/tiny/:integrationId", h.HandleTiny)

	// Instagram webhooks (Meta platform)
	instagram := app.Group("/api/webhooks/instagram")
	instagram.Get("/", h.HandleInstagramVerification)
	instagram.Post("/", h.HandleInstagramWebhook)
}

// HandleMercadoPagoOAuthCallback handles the OAuth callback from Mercado Pago.
// @Summary Handle Mercado Pago OAuth callback
// @Description Exchanges authorization code for access token and creates/updates integration
// @Tags webhooks
// @Produce json
// @Param code query string true "Authorization code"
// @Param state query string true "State parameter (contains store_id)"
// @Success 302 "Redirect to frontend with success"
// @Failure 302 "Redirect to frontend with error"
// @Router /api/webhooks/integrations/mercado_pago/oauth/callback [get]
func (h *WebhookHandler) HandleMercadoPagoOAuthCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	state := c.Query("state")

	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")

	if code == "" {
		h.logger.Error("OAuth callback missing code")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}

	if state == "" {
		h.logger.Error("OAuth callback missing state")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	h.logger.Info("mercado_pago OAuth callback received",
		zap.String("state", state),
		zap.Bool("has_code", code != ""),
	)

	output, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "mercado_pago",
		Code:     code,
		State:    state,
	})
	if err != nil {
		h.logger.Error("failed to handle OAuth callback",
			zap.String("state", state),
			zap.Error(err),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=oauth_failed", fiber.StatusFound)
	}

	h.logger.Info("mercado_pago OAuth completed successfully",
		zap.String("integration_id", output.IntegrationID),
		zap.String("store_id", output.StoreID),
	)

	// Redirect to frontend with success
	return c.Redirect(frontendURL+"/settings/integrations?success=mercado_pago_connected", fiber.StatusFound)
}

// HandleTinyOAuthCallback handles the OAuth callback from Tiny ERP.
// @Summary Handle Tiny OAuth callback
// @Description Exchanges authorization code for access token and creates/updates integration
// @Tags webhooks
// @Produce json
// @Param code query string true "Authorization code"
// @Param state query string true "State parameter (contains store_id)"
// @Success 302 "Redirect to frontend with success"
// @Failure 302 "Redirect to frontend with error"
// @Router /api/webhooks/integrations/tiny/oauth/callback [get]
func (h *WebhookHandler) HandleTinyOAuthCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	state := c.Query("state")

	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")

	if code == "" {
		h.logger.Error("Tiny OAuth callback missing code")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}

	if state == "" {
		h.logger.Error("Tiny OAuth callback missing state")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	h.logger.Info("tiny OAuth callback received",
		zap.String("state", state),
		zap.Bool("has_code", code != ""),
	)

	output, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "tiny",
		Code:     code,
		State:    state,
	})
	if err != nil {
		h.logger.Error("failed to handle Tiny OAuth callback",
			zap.String("state", state),
			zap.Error(err),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=oauth_failed", fiber.StatusFound)
	}

	h.logger.Info("tiny OAuth completed successfully",
		zap.String("integration_id", output.IntegrationID),
		zap.String("store_id", output.StoreID),
	)

	// Redirect to frontend with success
	return c.Redirect(frontendURL+"/settings/integrations?success=tiny_connected", fiber.StatusFound)
}

// HandleMercadoPago handles Mercado Pago webhook notifications.
// @Summary Handle Mercado Pago webhook
// @Description Receives and processes Mercado Pago payment notifications
// @Tags webhooks
// @Accept json
// @Produce json
// @Param integrationId path string true "Integration ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/integrations/mercado_pago/{integrationId} [post]
func (h *WebhookHandler) HandleMercadoPago(c *fiber.Ctx) error {
	integrationID := c.Params("integrationId")

	body := c.Body()

	// Parse Mercado Pago webhook payload
	var webhook struct {
		ID     int64  `json:"id"`
		Type   string `json:"type"`
		Action string `json:"action"`
		Data   struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &webhook); err != nil {
		h.logger.Error("failed to parse webhook payload",
			zap.String("integration_id", integrationID),
			zap.Error(err),
		)
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	h.logger.Info("mercado_pago webhook received",
		zap.String("integration_id", integrationID),
		zap.String("type", webhook.Type),
		zap.String("action", webhook.Action),
		zap.String("data_id", webhook.Data.ID),
	)

	// Store webhook event for audit trail
	eventID := webhook.Data.ID
	if eventID == "" {
		eventID = c.Get("X-Request-Id")
	}

	if err := h.service.StoreWebhookEvent(c.Context(), StoreWebhookInput{
		IntegrationID:  integrationID,
		Provider:       "mercado_pago",
		EventType:      webhook.Type,
		EventID:        eventID,
		Payload:        body,
		SignatureValid: true, // TODO: Implement signature verification
	}); err != nil {
		h.logger.Error("failed to store webhook event",
			zap.String("integration_id", integrationID),
			zap.Error(err),
		)
		// Don't return error - we still want to process the webhook
	}

	// Process payment notifications
	if webhook.Type == "payment" && webhook.Data.ID != "" {
		// Process asynchronously to respond quickly
		go func() {
			if err := h.service.ProcessPaymentNotification(c.Context(), ProcessPaymentInput{
				IntegrationID: integrationID,
				PaymentID:     webhook.Data.ID,
			}); err != nil {
				h.logger.Error("failed to process payment notification",
					zap.String("integration_id", integrationID),
					zap.String("payment_id", webhook.Data.ID),
					zap.Error(err),
				)
			}
		}()
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// HandleTiny handles Tiny ERP webhook notifications.
// @Summary Handle Tiny webhook
// @Description Receives and processes Tiny ERP notifications
// @Tags webhooks
// @Accept json
// @Produce json
// @Param integrationId path string true "Integration ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/integrations/tiny/{integrationId} [post]
func (h *WebhookHandler) HandleTiny(c *fiber.Ctx) error {
	integrationID := c.Params("integrationId")

	body := c.Body()

	// Always return 200 to Tiny — after 20 consecutive non-200 responses,
	// Tiny automatically removes the webhook URL.
	if len(body) == 0 {
		h.logger.Info("tiny webhook validation ping",
			zap.String("integration_id", integrationID),
		)
		return httpx.OK(c, fiber.Map{"status": "ok"})
	}

	// Parse Tiny V3 webhook payload
	// Real structure: {"versao":"1.0.1","cnpj":"...","tipo":"estoque","dados":{"idProduto":123,...}}
	var webhook struct {
		Tipo  string `json:"tipo"`
		Dados struct {
			IDProduto json.Number `json:"idProduto"`
			ID        string      `json:"id"`
			SKU       string      `json:"sku"`
			Nome      string      `json:"nome"`
			Saldo     *float64    `json:"saldo"`
		} `json:"dados"`
	}
	if err := json.Unmarshal(body, &webhook); err != nil {
		h.logger.Warn("failed to parse Tiny webhook payload",
			zap.String("integration_id", integrationID),
			zap.Error(err),
		)
	}

	// Resolve product ID: dados.idProduto (number) or dados.id (string)
	productID := webhook.Dados.IDProduto.String()
	if productID == "" {
		productID = webhook.Dados.ID
	}

	h.logger.Info("tiny webhook received",
		zap.String("integration_id", integrationID),
		zap.String("tipo", webhook.Tipo),
		zap.String("id_produto", productID),
		zap.String("sku", webhook.Dados.SKU),
	)

	// Store webhook event
	eventID := productID
	if eventID == "" {
		eventID = c.Get("X-Request-Id")
	}

	if err := h.service.StoreWebhookEvent(c.Context(), StoreWebhookInput{
		IntegrationID:  integrationID,
		Provider:       "tiny",
		EventType:      webhook.Tipo,
		EventID:        eventID,
		Payload:        json.RawMessage(body),
		SignatureValid: true, // Tiny doesn't use signatures
	}); err != nil {
		h.logger.Error("failed to store webhook event",
			zap.String("integration_id", integrationID),
			zap.Error(err),
		)
	}

	// Process product-related events: "estoque" (stock) and "produto" (product data)
	isProductEvent := webhook.Tipo == "estoque" || webhook.Tipo == "produto"
	if isProductEvent && productID != "" {
		go func() {
			ctx := context.Background()
			if err := h.service.ProcessProductWebhook(ctx, integrationID, productID); err != nil {
				h.logger.Error("failed to process product webhook",
					zap.String("integration_id", integrationID),
					zap.String("tipo", webhook.Tipo),
					zap.String("id_produto", productID),
					zap.Error(err),
				)
			}
		}()
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

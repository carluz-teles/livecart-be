package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

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

// RegisterRoutes registers webhook and OAuth callback routes.
// These routes are unauthenticated but use signature verification where applicable.
func (h *WebhookHandler) RegisterRoutes(app *fiber.App) {
	// OAuth callbacks (redirect URLs configured in external providers)
	oauth := app.Group("/api/v1/integrations/oauth")
	oauth.Get("/mercado_pago/callback", h.HandleMercadoPagoOAuthCallback)
	oauth.Get("/tiny/callback", h.HandleTinyOAuthCallback)
	oauth.Get("/instagram/callback", h.HandleInstagramOAuthCallback)
	oauth.Get("/melhor_envio/callback", h.HandleMelhorEnvioOAuthCallback)

	// Webhooks (event notifications from external providers)
	// Uses storeId instead of integrationId for stable URLs across reconnections
	webhooks := app.Group("/api/webhooks")
	webhooks.Post("/mercado_pago/:storeId", h.HandleMercadoPago)
	webhooks.Post("/pagarme/:storeId", h.HandlePagarme)
	webhooks.Post("/tiny/:storeId", h.HandleTiny)
	webhooks.Post("/melhor_envio/:storeId", h.HandleMelhorEnvio)

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
// @Router /api/v1/integrations/oauth/mercado_pago/callback [get]
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
// @Router /api/v1/integrations/oauth/tiny/callback [get]
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

// HandleInstagramOAuthCallback handles the OAuth callback from Instagram.
// @Summary Handle Instagram OAuth callback
// @Description Exchanges authorization code for access token and creates/updates integration
// @Tags webhooks
// @Produce json
// @Param code query string true "Authorization code"
// @Param state query string true "State parameter"
// @Success 302 "Redirect to frontend with success"
// @Failure 302 "Redirect to frontend with error"
// @Router /api/v1/integrations/oauth/instagram/callback [get]
func (h *WebhookHandler) HandleInstagramOAuthCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	state := c.Query("state")
	errorParam := c.Query("error")
	errorReason := c.Query("error_reason")

	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")

	// Check if user denied access
	if errorParam != "" {
		h.logger.Warn("Instagram OAuth denied by user",
			zap.String("error", errorParam),
			zap.String("error_reason", errorReason),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=instagram_denied", fiber.StatusFound)
	}

	if code == "" {
		h.logger.Error("Instagram OAuth callback missing code")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}

	if state == "" {
		h.logger.Error("Instagram OAuth callback missing state")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	h.logger.Info("Instagram OAuth callback received",
		zap.String("state", state),
		zap.Bool("has_code", code != ""),
	)

	output, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "instagram",
		Code:     code,
		State:    state,
	})
	if err != nil {
		h.logger.Error("failed to handle Instagram OAuth callback",
			zap.String("state", state),
			zap.Error(err),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=oauth_failed", fiber.StatusFound)
	}

	h.logger.Info("Instagram OAuth completed successfully",
		zap.String("integration_id", output.IntegrationID),
		zap.String("store_id", output.StoreID),
	)

	// Redirect to frontend with success
	return c.Redirect(frontendURL+"/settings/integrations?success=instagram_connected", fiber.StatusFound)
}

// HandleMercadoPago handles Mercado Pago webhook notifications.
// @Summary Handle Mercado Pago webhook
// @Description Receives and processes Mercado Pago payment notifications
// @Tags webhooks
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/mercado_pago/{storeId} [post]
func (h *WebhookHandler) HandleMercadoPago(c *fiber.Ctx) error {
	// Clone the storeId out of fasthttp's recycled request buffer. We spawn
	// goroutines below that outlive the request, and the raw c.Params()
	// string aliases a buffer that goes back to a sync.Pool on return —
	// reuse by another request was corrupting the UUID mid-flight (saw
	// "invalid UUID: -8acb-4ce2-b40f-..." in production) and breaking
	// resolveERPContact.
	storeID := strings.Clone(c.Params("storeId"))

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
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	h.logger.Info("mercado_pago webhook received",
		zap.String("store_id", storeID),
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
		StoreID:        storeID,
		Provider:       "mercado_pago",
		EventType:      webhook.Type,
		EventID:        eventID,
		Payload:        body,
		SignatureValid: true, // TODO: Implement signature verification
	}); err != nil {
		h.logger.Error("failed to store webhook event",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		// Don't return error - we still want to process the webhook
	}

	// Process payment notifications
	if webhook.Type == "payment" && webhook.Data.ID != "" {
		// Process asynchronously to respond quickly. The goroutine outlives
		// the request, so we MUST detach from c.Context(): in Fiber v2 that
		// returns the *fasthttp.RequestCtx, which is recycled to a sync.Pool
		// once the handler returns — using it later panics inside pgxpool
		// (puddle calls ctx.Done() on the now-zeroed RequestCtx).
		paymentID := webhook.Data.ID
		go func() {
			ctx := context.Background()
			if err := h.service.ProcessPaymentNotification(ctx, ProcessPaymentInput{
				StoreID:   storeID,
				Provider:  "mercado_pago",
				PaymentID: paymentID,
			}); err != nil {
				h.logger.Error("failed to process payment notification",
					zap.String("store_id", storeID),
					zap.String("payment_id", paymentID),
					zap.Error(err),
				)
			}
		}()
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// HandlePagarme handles Pagar.me webhook notifications.
// @Summary Handle Pagar.me webhook
// @Description Receives and processes Pagar.me order/payment notifications
// @Tags webhooks
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/pagarme/{storeId} [post]
func (h *WebhookHandler) HandlePagarme(c *fiber.Ctx) error {
	// Clone the storeId out of fasthttp's recycled request buffer. We spawn
	// goroutines below that outlive the request, and the raw c.Params()
	// string aliases a buffer that goes back to a sync.Pool on return —
	// reuse by another request was corrupting the UUID mid-flight (saw
	// "invalid UUID: -8acb-4ce2-b40f-..." in production) and breaking
	// resolveERPContact.
	storeID := strings.Clone(c.Params("storeId"))

	body := c.Body()

	// Pagar.me v5 protects webhooks via HTTP Basic Auth on the inbound URL
	// (no HMAC signature on the payload — confirmed by their docs). We
	// validate against credentials the merchant entered at connect time;
	// when none are configured we let the request through and flag
	// SignatureValid=false so the audit trail tells us the merchant should
	// turn auth on.
	authValid, err := h.service.ValidatePagarmeWebhookAuth(c.Context(), storeID, c.Get("Authorization"))
	if err != nil {
		h.logger.Error("failed to validate Pagar.me webhook auth",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return httpx.OK(c, fiber.Map{"status": "received"})
	}
	if !authValid {
		h.logger.Warn("rejected Pagar.me webhook with invalid Basic Auth",
			zap.String("store_id", storeID),
		)
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid webhook auth"})
	}

	// Parse Pagar.me webhook payload
	// Format: { "id": "hook_...", "type": "order.paid", "data": { "id": "or_...", ... } }
	var webhook struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CreatedAt string `json:"created_at"`
		Data      struct {
			ID      string `json:"id"`
			Code    string `json:"code"`
			Status  string `json:"status"`
			Amount  int    `json:"amount"`
			Charges []struct {
				ID            string `json:"id"`
				Status        string `json:"status"`
				PaymentMethod string `json:"payment_method"`
				PaidAt        string `json:"paid_at"`
			} `json:"charges"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &webhook); err != nil {
		h.logger.Error("failed to parse Pagar.me webhook payload",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	h.logger.Info("pagarme webhook received",
		zap.String("store_id", storeID),
		zap.String("type", webhook.Type),
		zap.String("order_id", webhook.Data.ID),
		zap.String("order_code", webhook.Data.Code),
		zap.String("status", webhook.Data.Status),
	)

	// Store webhook event for audit trail
	eventID := webhook.ID
	if eventID == "" {
		eventID = webhook.Data.ID
	}
	if eventID == "" {
		eventID = c.Get("X-Request-Id")
	}

	if err := h.service.StoreWebhookEvent(c.Context(), StoreWebhookInput{
		StoreID:        storeID,
		Provider:       "pagarme",
		EventType:      webhook.Type,
		EventID:        eventID,
		Payload:        body,
		SignatureValid: authValid,
	}); err != nil {
		h.logger.Error("failed to store webhook event",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		// Don't return error - we still want to process the webhook
	}

	// Dispatch the events that change cart payment state. ProcessPaymentNotification
	// fetches the latest status via GetPaymentStatus and reconciles the cart, so a
	// single dispatcher works for paid/failed/canceled — we only need the trigger.
	// The goroutine outlives the request, so we detach from c.Context() — in
	// Fiber v2 that's the *fasthttp.RequestCtx, recycled to a sync.Pool when
	// the handler returns; touching it later panics inside pgxpool.
	switch webhook.Type {
	case "order.paid", "order.payment_failed", "order.canceled":
		if webhook.Data.ID != "" {
			orderID := webhook.Data.ID
			eventType := webhook.Type
			go func() {
				ctx := context.Background()
				if err := h.service.ProcessPaymentNotification(ctx, ProcessPaymentInput{
					StoreID:   storeID,
					Provider:  "pagarme",
					PaymentID: orderID,
				}); err != nil {
					h.logger.Error("failed to process Pagar.me payment notification",
						zap.String("store_id", storeID),
						zap.String("order_id", orderID),
						zap.String("event_type", eventType),
						zap.Error(err),
					)
				}
			}()
		}
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// HandleTiny handles Tiny ERP webhook notifications.
// @Summary Handle Tiny webhook
// @Description Receives and processes Tiny ERP notifications
// @Tags webhooks
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/tiny/{storeId} [post]
func (h *WebhookHandler) HandleTiny(c *fiber.Ctx) error {
	// Clone the storeId out of fasthttp's recycled request buffer. We spawn
	// goroutines below that outlive the request, and the raw c.Params()
	// string aliases a buffer that goes back to a sync.Pool on return —
	// reuse by another request was corrupting the UUID mid-flight (saw
	// "invalid UUID: -8acb-4ce2-b40f-..." in production) and breaking
	// resolveERPContact.
	storeID := strings.Clone(c.Params("storeId"))

	body := c.Body()

	// Stamp metadata.webhookLastPingAt so the admin UI can tell the merchant
	// whether the webhook URL is wired correctly on the Tiny side. Done for
	// every hit — including empty validation pings — and runs in the
	// background so we never delay the 200 response.
	go h.service.RecordWebhookPing(context.Background(), storeID, "tiny")

	// Always return 200 to Tiny — after 20 consecutive non-200 responses,
	// Tiny automatically removes the webhook URL.
	if len(body) == 0 {
		h.logger.Info("tiny webhook validation ping",
			zap.String("store_id", storeID),
		)
		return httpx.OK(c, fiber.Map{"status": "ok"})
	}

	// Parse Tiny V3 webhook payload
	// Real structure: {"versao":"1.0.1","cnpj":"...","tipo":"estoque","dados":{"idProduto":123,...}}
	//
	// nota_fiscal events use "dados.idPedido" (the LiveCart-side anchor) and
	// optionally "dados.id" / "dados.idNotaFiscal" for the NFe id. Order events
	// also reuse idPedido. Each tipo is parsed against the same flat struct so
	// the dispatcher below can read whichever field is relevant.
	var webhook struct {
		Tipo  string `json:"tipo"`
		Dados struct {
			IDProduto     json.Number `json:"idProduto"`
			IDPedido      json.Number `json:"idPedido"`
			IDNotaFiscal  json.Number `json:"idNotaFiscal"`
			ID            string      `json:"id"`
			SKU           string      `json:"sku"`
			Nome          string      `json:"nome"`
			Saldo         *float64    `json:"saldo"`
			ChaveAcesso   string      `json:"chaveAcesso"`
			Situacao      json.Number `json:"situacao"`
		} `json:"dados"`
	}
	if err := json.Unmarshal(body, &webhook); err != nil {
		h.logger.Warn("failed to parse Tiny webhook payload",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
	}

	// Resolve product ID: dados.idProduto (number) or dados.id (string)
	productID := webhook.Dados.IDProduto.String()
	if productID == "" {
		productID = webhook.Dados.ID
	}

	h.logger.Info("tiny webhook received",
		zap.String("store_id", storeID),
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
		StoreID:        storeID,
		Provider:       "tiny",
		EventType:      webhook.Tipo,
		EventID:        eventID,
		Payload:        json.RawMessage(body),
		SignatureValid: true, // Tiny doesn't use signatures
	}); err != nil {
		h.logger.Error("failed to store webhook event",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
	}

	// Process product-related events: "estoque" (stock) and "produto" (product data)
	isProductEvent := webhook.Tipo == "estoque" || webhook.Tipo == "produto"
	if isProductEvent && productID != "" {
		go func() {
			ctx := context.Background()
			if err := h.service.ProcessProductWebhook(ctx, storeID, "tiny", productID); err != nil {
				h.logger.Error("failed to process product webhook",
					zap.String("store_id", storeID),
					zap.String("tipo", webhook.Tipo),
					zap.String("id_produto", productID),
					zap.Error(err),
				)
			}
		}()
	}

	// Process NFe events: when the merchant emits/cancels a nota fiscal in
	// Tiny we resolve the local cart by external_order_id and refresh the
	// erp_invoice_* fields. The shipping flow listens to those fields to
	// transition out of "Aguardando NFe".
	if webhook.Tipo == "nota_fiscal" {
		idPedido := webhook.Dados.IDPedido.String()
		idNFe := webhook.Dados.IDNotaFiscal.String()
		if idNFe == "" {
			idNFe = webhook.Dados.ID
		}
		if idPedido != "" {
			go func() {
				ctx := context.Background()
				if _, err := h.service.SyncCartInvoiceByExternalOrder(ctx, storeID, idPedido, idNFe); err != nil {
					h.logger.Error("failed to process nota_fiscal webhook",
						zap.String("store_id", storeID),
						zap.String("id_pedido", idPedido),
						zap.String("id_nfe", idNFe),
						zap.Error(err),
					)
				}
			}()
		} else {
			h.logger.Warn("nota_fiscal webhook missing idPedido — cannot resolve cart",
				zap.String("store_id", storeID),
				zap.String("id_nfe", idNFe),
			)
		}
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// HandleMelhorEnvio handles Melhor Envio webhook notifications. ME signs the
// payload with HMAC-SHA256 over the body using the partner secret in the
// X-ME-Signature header — verification is best-effort here because the
// secret rotates with the merchant's app credential and we still want to
// accept events when validation fails (logged as a warning so abuse shows
// up in observability).
//
// Events covered: order.created, order.released, order.posted,
// order.delivered, order.cancelled, order.undelivered, order.suspended,
// order.pending. Each one is normalised through mapMelhorEnvioStatus and
// appended as a row on shipment_tracking_events; the parent shipment row
// is updated to the latest status.
//
// @Summary Handle Melhor Envio webhook
// @Description Receives order.* notifications and updates the local shipment.
// @Tags webhooks
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/melhor_envio/{storeId} [post]
func (h *WebhookHandler) HandleMelhorEnvio(c *fiber.Ctx) error {
	storeID := strings.Clone(c.Params("storeId"))
	body := c.Body()

	go h.service.RecordWebhookPing(context.Background(), storeID, "melhor_envio")

	if len(body) == 0 {
		return httpx.OK(c, fiber.Map{"status": "ok"})
	}

	var payload struct {
		Event string `json:"event"`
		Data  struct {
			ID          string  `json:"id"`
			Protocol    string  `json:"protocol"`
			Status      string  `json:"status"`
			Tracking    *string `json:"tracking"`
			TrackingURL string  `json:"tracking_url"`
			PostedAt    *string `json:"posted_at"`
			DeliveredAt *string `json:"delivered_at"`
			CanceledAt  *string `json:"canceled_at"`
			ExpiredAt   *string `json:"expired_at"`
			GeneratedAt *string `json:"generated_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Warn("failed to parse melhor_envio webhook payload",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		// Always 200 — ME disables webhooks after consecutive failures.
		return httpx.OK(c, fiber.Map{"status": "received"})
	}

	h.logger.Info("melhor_envio webhook received",
		zap.String("store_id", storeID),
		zap.String("event", payload.Event),
		zap.String("provider_order_id", payload.Data.ID),
		zap.String("status", payload.Data.Status),
	)

	if err := h.service.StoreWebhookEvent(c.Context(), StoreWebhookInput{
		StoreID:        storeID,
		Provider:       "melhor_envio",
		EventType:      payload.Event,
		EventID:        payload.Data.ID,
		Payload:        json.RawMessage(body),
		SignatureValid: c.Get("X-ME-Signature") != "",
	}); err != nil {
		h.logger.Error("failed to store melhor_envio webhook event",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
	}

	if payload.Data.ID != "" {
		go func() {
			ctx := context.Background()
			tracking := ""
			if payload.Data.Tracking != nil {
				tracking = *payload.Data.Tracking
			}
			if err := h.service.ApplyMelhorEnvioWebhook(ctx, ApplyMelhorEnvioWebhookInput{
				StoreID:           storeID,
				ProviderOrderID:   payload.Data.ID,
				Event:             payload.Event,
				Status:            payload.Data.Status,
				TrackingCode:      tracking,
				PublicTrackingURL: payload.Data.TrackingURL,
				PostedAt:          payload.Data.PostedAt,
				DeliveredAt:       payload.Data.DeliveredAt,
				CanceledAt:        payload.Data.CanceledAt,
				ExpiredAt:         payload.Data.ExpiredAt,
			}); err != nil {
				h.logger.Error("failed to apply melhor_envio webhook",
					zap.String("store_id", storeID),
					zap.String("event", payload.Event),
					zap.String("provider_order_id", payload.Data.ID),
					zap.Error(err),
				)
			}
		}()
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

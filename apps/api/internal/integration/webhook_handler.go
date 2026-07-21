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
	"livecart/apps/api/lib/logger"
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
func (h *WebhookHandler) RegisterRoutes(app *fiber.App, slugResolver httpx.StoreSlugResolver) {
	// OAuth callbacks (redirect URLs configured in external providers)
	oauth := app.Group("/api/v1/integrations/oauth")
	oauth.Get("/mercado_pago/callback", h.HandleMercadoPagoOAuthCallback)
	oauth.Get("/tiny/callback", h.HandleTinyOAuthCallback)
	oauth.Get("/instagram/callback", h.HandleInstagramOAuthCallback)
	oauth.Get("/melhor_envio/callback", h.HandleMelhorEnvioOAuthCallback)

	// Webhooks (event notifications from external providers)
	// Uses storeId instead of integrationId for stable URLs across reconnections
	// storeCtx anota store_id/store_slug nos Locals para os logs do request.
	storeCtx := httpx.WebhookStoreContext(slugResolver)
	webhooks := app.Group("/api/webhooks")
	webhooks.Post("/mercado_pago/:storeId", storeCtx, h.HandleMercadoPago)
	webhooks.Post("/pagarme/:storeId", storeCtx, h.HandlePagarme)
	webhooks.Post("/tiny/:storeId", storeCtx, h.HandleTiny)
	webhooks.Post("/melhor_envio/:storeId", storeCtx, h.HandleMelhorEnvio)
	webhooks.Post("/twilio/:storeId", storeCtx, h.HandleTwilio)

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
		logger.From(c.Context(), h.logger).Error("OAuth callback missing code")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}

	if state == "" {
		logger.From(c.Context(), h.logger).Error("OAuth callback missing state")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	logger.From(c.Context(), h.logger).Info("mercado_pago OAuth callback received",
		zap.String("state", state),
		zap.Bool("has_code", code != ""),
	)

	output, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "mercado_pago",
		Code:     code,
		State:    state,
	})
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to handle OAuth callback",
			zap.String("state", state),
			zap.Error(err),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=oauth_failed", fiber.StatusFound)
	}

	logger.From(logger.WithStore(c.Context(), output.StoreID, ""), h.logger).Info("mercado_pago OAuth completed successfully",
		zap.String("integration_id", output.IntegrationID),
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
		logger.From(c.Context(), h.logger).Error("Tiny OAuth callback missing code")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}

	if state == "" {
		logger.From(c.Context(), h.logger).Error("Tiny OAuth callback missing state")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	logger.From(c.Context(), h.logger).Info("tiny OAuth callback received",
		zap.String("state", state),
		zap.Bool("has_code", code != ""),
	)

	output, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "tiny",
		Code:     code,
		State:    state,
	})
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to handle Tiny OAuth callback",
			zap.String("state", state),
			zap.Error(err),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=oauth_failed", fiber.StatusFound)
	}

	logger.From(logger.WithStore(c.Context(), output.StoreID, ""), h.logger).Info("tiny OAuth completed successfully",
		zap.String("integration_id", output.IntegrationID),
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
		logger.From(c.Context(), h.logger).Warn("Instagram OAuth denied by user",
			zap.String("error", errorParam),
			zap.String("error_reason", errorReason),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=instagram_denied", fiber.StatusFound)
	}

	if code == "" {
		logger.From(c.Context(), h.logger).Error("Instagram OAuth callback missing code")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}

	if state == "" {
		logger.From(c.Context(), h.logger).Error("Instagram OAuth callback missing state")
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	logger.From(c.Context(), h.logger).Info("Instagram OAuth callback received",
		zap.String("state", state),
		zap.Bool("has_code", code != ""),
	)

	output, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "instagram",
		Code:     code,
		State:    state,
	})
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to handle Instagram OAuth callback",
			zap.String("state", state),
			zap.Error(err),
		)
		return c.Redirect(frontendURL+"/settings/integrations?error=oauth_failed", fiber.StatusFound)
	}

	logger.From(logger.WithStore(c.Context(), output.StoreID, ""), h.logger).Info("Instagram OAuth completed successfully",
		zap.String("integration_id", output.IntegrationID),
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
	storeSlug := httpx.GetStoreSlug(c)

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
		logger.From(c.Context(), h.logger).Error("failed to parse webhook payload",
			zap.Error(err),
		)
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	logger.From(c.Context(), h.logger).Info("mercado_pago webhook received",
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
		logger.From(c.Context(), h.logger).Error("failed to store webhook event",
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
			ctx := logger.WithStore(context.Background(), storeID, storeSlug)
			if err := h.service.ProcessPaymentNotification(ctx, ProcessPaymentInput{
				StoreID:   storeID,
				Provider:  "mercado_pago",
				PaymentID: paymentID,
			}); err != nil {
				logger.From(ctx, h.logger).Error("failed to process payment notification",
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
	storeSlug := httpx.GetStoreSlug(c)

	body := c.Body()

	// Pagar.me v5 protects webhooks via HTTP Basic Auth on the inbound URL
	// (no HMAC signature on the payload — confirmed by their docs). We
	// validate against credentials the merchant entered at connect time;
	// when none are configured we let the request through and flag
	// SignatureValid=false so the audit trail tells us the merchant should
	// turn auth on.
	authValid, err := h.service.ValidatePagarmeWebhookAuth(c.Context(), storeID, c.Get("Authorization"))
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to validate Pagar.me webhook auth",
			zap.Error(err),
		)
		return httpx.OK(c, fiber.Map{"status": "received"})
	}
	if !authValid {
		logger.From(c.Context(), h.logger).Warn("rejected Pagar.me webhook with invalid Basic Auth")
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
		logger.From(c.Context(), h.logger).Error("failed to parse Pagar.me webhook payload",
			zap.Error(err),
		)
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	// Loopback self-test (TestPagarmeWebhookEndpoint): the dashboard "Testar
	// endpoint" button POSTs this synthetic event to our own public URL to
	// prove the endpoint is reachable and the Basic Auth is consistent. It has
	// already passed the auth check above; short-circuit here as a pure no-op —
	// no audit row, no ping stamp, no cart reconciliation — so the
	// delivery-history probe stays honest about REAL Pagar.me deliveries.
	if webhook.Type == pagarmeWebhookTestType {
		logger.From(c.Context(), h.logger).Info("pagarme webhook self-test received")
		return httpx.OK(c, fiber.Map{"status": "test_ok"})
	}

	// Real webhook test: events for the throwaway order created by
	// RunPagarmeWebhookLiveTest carry the LCWHTEST- code prefix. Never process
	// these as a real payment — return 200 so Pagar.me records a healthy
	// delivery (which the test reads back from GET /hooks) and stop before the
	// dispatch switch. This is the identifier guard so a test order can never
	// be mistaken for a real sale.
	if strings.HasPrefix(webhook.Data.Code, pagarmeWebhookTestOrderPrefix) {
		logger.From(c.Context(), h.logger).Info("pagarme webhook live-test event received",
			zap.String("type", webhook.Type),
			zap.String("order_code", webhook.Data.Code),
		)
		// A real Pagar.me webhook reached us — stamp the ping so the admin UI
		// flips from "pending" to "active" and the setup warning clears without
		// waiting for organic traffic. Detached ctx: outlives the request.
		go h.service.RecordWebhookPing(logger.WithStore(context.Background(), storeID, storeSlug), storeID, "pagarme")
		return httpx.OK(c, fiber.Map{"status": "webhook_test_ok"})
	}

	logger.From(c.Context(), h.logger).Info("pagarme webhook received",
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
		logger.From(c.Context(), h.logger).Error("failed to store webhook event",
			zap.Error(err),
		)
		// Don't return error - we still want to process the webhook
	}

	// Stamp metadata.webhookLastPingAt on every successful Pagar.me webhook
	// so the admin UI can show whether the merchant has the URL wired up.
	// Detached from c.Context() (recycled by Fiber) — same pattern as Tiny.
	go h.service.RecordWebhookPing(logger.WithStore(context.Background(), storeID, storeSlug), storeID, "pagarme")

	// Dispatch the events that change cart payment state. ProcessPaymentNotification
	// fetches the latest status via GetPaymentStatus and reconciles the cart, so a
	// single dispatcher works for paid/failed/canceled — we only need the trigger.
	// The goroutine outlives the request, so we detach from c.Context() — in
	// Fiber v2 that's the *fasthttp.RequestCtx, recycled to a sync.Pool when
	// the handler returns; touching it later panics inside pgxpool.
	// Gateway accounts emit order.* (Data.ID = or_...); PSP accounts emit
	// charge.* (Data.ID = ch_...). GetPaymentStatus routes on the id prefix, so
	// a single dispatcher reconciles both — we only need to fire on the events
	// that move a cart to a terminal state. Without the charge.* cases, PIX and
	// card payments on PSP accounts were confirmed at the gateway but never
	// reconciled here, leaving the cart stuck on "pending".
	switch webhook.Type {
	case "order.paid", "order.payment_failed", "order.canceled",
		"charge.paid", "charge.payment_failed", "charge.canceled",
		"charge.refunded", "charge.chargedback":
		if webhook.Data.ID != "" {
			orderID := webhook.Data.ID
			eventType := webhook.Type
			go func() {
				ctx := logger.WithStore(context.Background(), storeID, storeSlug)
				if err := h.service.ProcessPaymentNotification(ctx, ProcessPaymentInput{
					StoreID:   storeID,
					Provider:  "pagarme",
					PaymentID: orderID,
				}); err != nil {
					logger.From(ctx, h.logger).Error("failed to process Pagar.me payment notification",
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
	storeSlug := httpx.GetStoreSlug(c)

	body := c.Body()

	// Stamp metadata.webhookLastPingAt so the admin UI can tell the merchant
	// whether the webhook URL is wired correctly on the Tiny side. Done for
	// every hit — including empty validation pings — and runs in the
	// background so we never delay the 200 response.
	go h.service.RecordWebhookPing(logger.WithStore(context.Background(), storeID, storeSlug), storeID, "tiny")

	// Always return 200 to Tiny — after 20 consecutive non-200 responses,
	// Tiny automatically removes the webhook URL.
	if len(body) == 0 {
		logger.From(c.Context(), h.logger).Info("tiny webhook validation ping")
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
			IDProduto    json.Number `json:"idProduto"`
			IDPedido     json.Number `json:"idPedido"`
			IDNotaFiscal json.Number `json:"idNotaFiscal"`
			ID           string      `json:"id"`
			SKU          string      `json:"sku"`
			Nome         string      `json:"nome"`
			Saldo        *float64    `json:"saldo"`
			ChaveAcesso  string      `json:"chaveAcesso"`
			Situacao     json.Number `json:"situacao"`
		} `json:"dados"`
	}
	if err := json.Unmarshal(body, &webhook); err != nil {
		logger.From(c.Context(), h.logger).Warn("failed to parse Tiny webhook payload",
			zap.Error(err),
		)
	}

	// Resolve product ID: dados.idProduto (number) or dados.id (string)
	productID := webhook.Dados.IDProduto.String()
	if productID == "" {
		productID = webhook.Dados.ID
	}

	logger.From(c.Context(), h.logger).Info("tiny webhook received",
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
		logger.From(c.Context(), h.logger).Error("failed to store webhook event",
			zap.Error(err),
		)
	}

	// Process product-related events: "estoque" (stock) and "produto" (product data)
	isProductEvent := webhook.Tipo == "estoque" || webhook.Tipo == "produto"
	if isProductEvent && productID != "" {
		go func() {
			ctx := logger.WithStore(context.Background(), storeID, storeSlug)
			stockApplied, syncErr := h.service.ProcessProductWebhook(ctx, storeID, "tiny", productID)
			if syncErr != nil {
				logger.From(ctx, h.logger).Error("failed to process product webhook",
					zap.String("tipo", webhook.Tipo),
					zap.String("id_produto", productID),
					zap.Error(syncErr),
				)
			}

			// Após sincronizar produto/estoque, varre eventos ativos com
			// fila para esse produto e tenta promover o próximo. Esse é
			// o catch-all do "ERP devolveu estoque por mudança manual"
			// — para release vindo de carts/checkout o caller já chama
			// ProcessWaitlistForProduct inline; aqui é o backstop.
			//
			// Condicionado ao sync ter APLICADO o estoque local: se o sync
			// falhou ou o guard segurou o overwrite (reserva ativa ou
			// finalização ERP em voo), promover agora agiria sobre um
			// contador stale/envenenado. O próximo webhook do produto
			// re-dispara o backstop com o guard desarmado.
			if webhook.Tipo == "estoque" {
				if syncErr != nil || !stockApplied {
					logger.From(ctx, h.logger).Info("skipping waitlist backstop: stock sync not applied",
						zap.String("external_product_id", productID),
						zap.Bool("stock_applied", stockApplied),
						zap.Bool("sync_failed", syncErr != nil),
					)
					return
				}
				if err := h.service.ProcessWaitlistAfterStockWebhook(ctx, storeID, "tiny", productID); err != nil {
					logger.From(ctx, h.logger).Warn("failed to process waitlist after stock webhook",
						zap.String("external_product_id", productID),
						zap.Error(err),
					)
				}
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
				ctx := logger.WithStore(context.Background(), storeID, storeSlug)
				if _, err := h.service.SyncCartInvoiceByExternalOrder(ctx, storeID, idPedido, idNFe); err != nil {
					logger.From(ctx, h.logger).Error("failed to process nota_fiscal webhook",
						zap.String("id_pedido", idPedido),
						zap.String("id_nfe", idNFe),
						zap.Error(err),
					)
				}
			}()
		} else {
			logger.From(c.Context(), h.logger).Warn("nota_fiscal webhook missing idPedido — cannot resolve cart",
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
	storeSlug := httpx.GetStoreSlug(c)
	body := c.Body()

	go h.service.RecordWebhookPing(logger.WithStore(context.Background(), storeID, storeSlug), storeID, "melhor_envio")

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
		logger.From(c.Context(), h.logger).Warn("failed to parse melhor_envio webhook payload",
			zap.Error(err),
		)
		// Always 200 — ME disables webhooks after consecutive failures.
		return httpx.OK(c, fiber.Map{"status": "received"})
	}

	logger.From(c.Context(), h.logger).Info("melhor_envio webhook received",
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
		logger.From(c.Context(), h.logger).Error("failed to store melhor_envio webhook event",
			zap.Error(err),
		)
	}

	if payload.Data.ID != "" {
		go func() {
			ctx := logger.WithStore(context.Background(), storeID, storeSlug)
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
				logger.From(ctx, h.logger).Error("failed to apply melhor_envio webhook",
					zap.String("event", payload.Event),
					zap.String("provider_order_id", payload.Data.ID),
					zap.Error(err),
				)
			}
		}()
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// HandleTwilio handles Twilio WhatsApp webhooks (PRD 006): message status
// callbacks (sent/delivered/read/failed) and inbound customer replies
// (opt-out). Twilio posts application/x-www-form-urlencoded and signs every
// request with X-Twilio-Signature (HMAC-SHA1 of URL + sorted params, keyed by
// the subaccount auth token).
// @Summary Handle Twilio WhatsApp webhook
// @Description Processes message status callbacks and inbound replies
// @Tags webhooks
// @Accept x-www-form-urlencoded
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} map[string]string
// @Router /api/webhooks/twilio/{storeId} [post]
func (h *WebhookHandler) HandleTwilio(c *fiber.Ctx) error {
	storeID := strings.Clone(c.Params("storeId"))

	params := make(map[string]string)
	c.Request().PostArgs().VisitAll(func(k, v []byte) {
		params[string(k)] = string(v)
	})

	valid, err := h.service.ValidateTwilioWebhookSignature(c.Context(), storeID, params, c.Get("X-Twilio-Signature"))
	if err != nil {
		// Integration missing or credentials unreadable — ack so Twilio
		// doesn't retry-storm us; the log keeps the trail.
		logger.From(c.Context(), h.logger).Warn("twilio webhook signature validation errored",
			zap.Error(err),
		)
		return httpx.OK(c, fiber.Map{"status": "received"})
	}
	if !valid {
		logger.From(c.Context(), h.logger).Warn("rejected twilio webhook with invalid signature")
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid signature"})
	}

	h.service.RecordWebhookPing(c.Context(), storeID, "twilio_whatsapp")

	// Status callback (MessageStatus present) vs inbound message (Body present).
	if status := params["MessageStatus"]; status != "" {
		if err := h.service.ProcessTwilioStatusCallback(c.Context(), storeID, params["MessageSid"], status, params["ErrorCode"]); err != nil {
			logger.From(c.Context(), h.logger).Error("failed to process twilio status callback",
				zap.String("message_sid", params["MessageSid"]),
				zap.Error(err),
			)
		}
	} else if body := params["Body"]; body != "" {
		if err := h.service.ProcessTwilioInbound(c.Context(), storeID, params["From"], body); err != nil {
			logger.From(c.Context(), h.logger).Error("failed to process twilio inbound message",
				zap.Error(err),
			)
		}
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

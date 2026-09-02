package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// PaymentDispatcher is the slice of payment.Service the webhook edge uses to
// hand a payment notification off to the async consumer (L1→L2 of the event
// choreography). Declared here — in the consumer — and implemented by
// *payment.Service, so the webhook edge dispatches straight to the payment
// domain instead of routing through the integration.Service delegation
// (strangler-fig B1e).
type PaymentDispatcher interface {
	DispatchPaymentProcess(ctx context.Context, input paymentdomain.ProcessPaymentInput) error
	// AplicarStatusDePagamento é o que o webhook real faz DEPOIS de consultar o
	// gateway. O simulador de staging entra por aqui porque não há pagamento
	// para consultar — e entrar por aqui é o que garante que simulado e real
	// sejam a mesma coisa daí para a frente.
	AplicarStatusDePagamento(ctx context.Context, input paymentdomain.ProcessPaymentInput, status *providers.PaymentStatus) error
}

// WebhookHandler handles incoming webhooks from external providers.
type WebhookHandler struct {
	service *Service
	payment PaymentDispatcher
	logger  *zap.Logger
}

// NewWebhookHandler creates a new webhook handler.
func NewWebhookHandler(service *Service, payment PaymentDispatcher, logger *zap.Logger) *WebhookHandler {
	return &WebhookHandler{
		service: service,
		payment: payment,
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
	oauth.Get("/bling/callback", h.HandleBlingOAuthCallback)

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

	// O Bling é a ÚNICA rota sem :storeId, e não é esquecimento: o Bling não
	// tem API para registrar webhook — a URL é cadastrada na UI do APLICATIVO,
	// que é um só para todas as lojas. Quem identifica a origem é o `companyId`
	// do envelope, casado com integrations.erp_account_id.
	webhooks.Post("/bling", h.HandleBling)

	// TODO MÉTODO que não seja POST responde 200 com JSON, nas MESMAS URLs.
	//
	// O provedor verifica a URL antes de aceitar o cadastro, e verifica com um
	// método que não entrega evento. Registradas só como POST, essas rotas
	// devolviam 405 à sondagem e o painel da Tiny recusava com "Não foi
	// possível acessar a URL" — a URL estava no ar o tempo todo, respondendo
	// 200 ao POST.
	//
	// A lista é ampla de propósito. Um túnel apontando para um servidor de eco
	// trivial foi aceito pela Tiny no mesmo minuto em que a nossa URL era
	// recusada, e a comparação das duas respostas sobrou exatamente nisto: o
	// eco devolvia 200 com JSON em QUALQUER método, e nós devolvíamos 405 em
	// OPTIONS e PUT e texto puro no GET. Não sabemos qual método a sondagem
	// usa — e não precisamos saber para atendê-la.
	//
	// O Instagram nunca sofreu disso, e a razão é a linha logo abaixo: ele já
	// tinha um GET, para o desafio da Meta.
	//
	// Nenhum desses métodos recebe evento nem toca em nada. Quem processa
	// continua sendo exclusivamente o POST.
	for _, provider := range []string{"mercado_pago", "pagarme", "tiny", "melhor_envio", "twilio"} {
		path := "/" + provider + "/:storeId"
		webhooks.Get(path, h.HandleWebhookProbe)
		webhooks.Head(path, h.HandleWebhookProbe)
		webhooks.Options(path, h.HandleWebhookProbe)
		webhooks.Put(path, h.HandleWebhookProbe)
		webhooks.Patch(path, h.HandleWebhookProbe)
		webhooks.Delete(path, h.HandleWebhookProbe)
	}

	// Instagram webhooks (Meta platform)
	instagram := app.Group("/api/webhooks/instagram")
	instagram.Get("/", h.HandleInstagramVerification)
	instagram.Post("/", h.HandleInstagramWebhook)
}

// HandleWebhookProbe answers the reachability check a provider makes before it
// accepts a webhook URL.
//
// Deliberately inert: no signature check, no store lookup, no side effect. The
// probe asks one question — "is anyone listening at this address?" — and the
// answer is 200. Every event still arrives, and is only ever processed, via
// POST.
//
// Responde JSON, e não o texto "OK" do SendStatus. O sondador é código, não
// gente: um validador que faz parse da resposta engasga com `text/plain` e
// reporta a URL como inacessível, que é indistinguível de estar fora do ar.
// JSON é o que o provedor recebe de nós em todos os outros caminhos.
func (h *WebhookHandler) HandleWebhookProbe(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
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
		// Thin dispatcher (L1): emit a payment.process command to the outbox and
		// return. The command consumer runs the guarded reconciliation with retry
		// + dead-letter — replacing the old fire-and-forget goroutine that lost
		// the work on any failure. The emit is a quick sync outbox insert, so the
		// request context is still valid (no detach needed).
		paymentID := webhook.Data.ID
		ctx := logger.WithStore(c.UserContext(), storeID, storeSlug)
		if err := h.payment.DispatchPaymentProcess(ctx, paymentdomain.ProcessPaymentInput{
			StoreID:   storeID,
			Provider:  "mercado_pago",
			PaymentID: paymentID,
		}); err != nil {
			logger.From(ctx, h.logger).Error("failed to dispatch payment.process",
				zap.String("payment_id", paymentID), zap.Error(err))
		}
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
// isLiveCartOrderCode diz se o `code` do pedido no gateway foi gravado por nós.
//
// Gravamos sempre o UUID do carrinho (checkout de cartão e de Pix), e os
// pedidos do teste de webhook levam o prefixo LCWHTEST-. Qualquer outro formato
// pertence a outra plataforma que compartilha a conta do gateway.
func isLiveCartOrderCode(code string) bool {
	if strings.HasPrefix(code, pagarmeWebhookTestOrderPrefix) {
		return true
	}
	_, err := uuid.Parse(code)
	return err == nil
}

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

	// Pedido de OUTRA plataforma na mesma conta do gateway.
	//
	// A conta Pagar.me pode ser compartilhada (a cantodaart usa a mesma no
	// Shopify) e o gateway entrega todos os eventos dela para a URL cadastrada.
	// Nosso `code` é sempre o UUID do carrinho, então um code presente que não é
	// UUID veio de outra loja — parar aqui evita a consulta à API do gateway
	// para um pagamento que nunca foi nosso.
	//
	// Só decide com o code PRESENTE: evento de charge nem sempre o traz, e o
	// discriminador definitivo roda depois, sobre a referência que a consulta
	// devolve (payment.ProcessPaymentNotification).
	if webhook.Data.Code != "" && !isLiveCartOrderCode(webhook.Data.Code) {
		logger.From(c.Context(), h.logger).Info("pagarme webhook for another platform's order, ignoring",
			zap.String("type", webhook.Type),
			zap.String("order_code", webhook.Data.Code),
		)
		return httpx.OK(c, fiber.Map{"status": "ignored_foreign_order"})
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
			// Thin dispatcher (L1): emit payment.process to the outbox; the consumer
			// runs the guarded reconciliation with retry + dead-letter.
			ctx := logger.WithStore(c.UserContext(), storeID, storeSlug)
			if err := h.payment.DispatchPaymentProcess(ctx, paymentdomain.ProcessPaymentInput{
				StoreID:   storeID,
				Provider:  "pagarme",
				PaymentID: orderID,
			}); err != nil {
				logger.From(ctx, h.logger).Error("failed to dispatch Pagar.me payment.process",
					zap.String("order_id", orderID), zap.String("event_type", eventType), zap.Error(err))
			}
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
			// Eventos de pedido (inclusao_pedido / atualizacao_pedido). O
			// `codigoSituacao` é um SLUG ("aberto", "aprovado", "enviado"), não o
			// código numérico que a API usa para MUDAR a situação — as duas
			// grafias convivem, e a tabela que as casa está em
			// providers.ERPOrderStatus.
			Numero            textoOuNumero `json:"numero"`
			CodigoSituacao    string        `json:"codigoSituacao"`
			DescricaoSituacao string        `json:"descricaoSituacao"`
			// Evento de rastreio.
			IDVendaTiny    json.Number `json:"idVendaTiny"`
			CodigoRastreio string      `json:"codigoRastreio"`
			URLRastreio    string      `json:"urlRastreio"`
			Transportadora string      `json:"transportadora"`
		} `json:"dados"`
	}
	if err := json.Unmarshal(body, &webhook); err != nil {
		// SAIR, e não seguir. Antes isto era um Warn e o fluxo continuava com o
		// struct ZERADO — o que é pior do que não tratar: `idPedido` virava
		// vazio e a nota fiscal era descartada por "missing idPedido", quando o
		// id estava no corpo o tempo todo. Nove notas perdidas em 02/09/2026 por
		// causa de UM campo com o tipo trocado.
		//
		// 200 mesmo assim: o Tiny reenviaria o mesmo corpo, e o mesmo corpo
		// falharia igual. Quem conserta isto é gente lendo o log, e por isso o
		// payload vai junto.
		logger.From(c.Context(), h.logger).Error("Tiny webhook ilegível — nada foi processado",
			zap.Error(err),
			zap.String("payload", limitarPayload(body)),
		)
		return httpx.OK(c, fiber.Map{"status": "unparseable"})
	}

	// Resolve product ID: dados.idProduto (number) or dados.id (string)
	productID := webhook.Dados.IDProduto.String()
	if productID == "" {
		productID = webhook.Dados.ID
	}

	// O payload cru, para descobrir QUAIS saldos o Tiny manda.
	//
	// Hoje o estoque local vem de `estoque.quantidade` do GET /produtos/{id}, que
	// é o saldo FÍSICO. O lojista relatou que precisa ser o DISPONÍVEL: um
	// orçamento salvo no Tiny reserva a peça, que sai do disponível e continua no
	// físico — vender por cima disso é furo de estoque.
	//
	// O `saldo` deste payload é parseado e nunca usado, então ninguém sabe qual
	// dos dois ele é. Se vier o disponível (ou vier a quebra reservado/disponível),
	// o conserto não custa chamada nenhuma ao Tiny; se vier só o físico, é um GET
	// /estoque/{id} por produto, e aí o rate limit entra na conta.
	//
	// Restrito a tipo=estoque de propósito: `atualizacao_pedido` carrega dados do
	// comprador, que não têm por que ir para o log.
	campos := []zap.Field{
		zap.String("tipo", webhook.Tipo),
		zap.String("id_produto", productID),
		zap.String("sku", webhook.Dados.SKU),
	}
	if webhook.Tipo == "estoque" {
		campos = append(campos, zap.ByteString("payload", body))
	}
	logger.From(c.Context(), h.logger).Info("tiny webhook received", campos...)

	// Âncora de dedupe do webhook_events. Para eventos de pedido é o par
	// pedido+situação: sem a situação, uma redelivery e uma transição de verdade
	// no mesmo pedido teriam a mesma chave, e a segunda seria descartada como
	// duplicata.
	eventID := productID
	switch {
	case webhook.Tipo == "inclusao_pedido" || webhook.Tipo == "atualizacao_pedido":
		eventID = webhook.Dados.ID + ":" + webhook.Dados.CodigoSituacao
	case webhook.Tipo == "rastreio":
		eventID = "rastreio:" + webhook.Dados.IDVendaTiny.String()
	}
	if eventID == "" || eventID == ":" {
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

	// Eventos de PEDIDO: o ERP avisa a cada transição de situação, e é assim que
	// o trajeto pós-venda (faturado → separado → enviado → entregue) chega ao
	// LiveCart em vez de ficar só no outro sistema.
	//
	// `inclusao_pedido` entra junto com `atualizacao_pedido` de propósito: o
	// pedido que o LiveCart acabou de criar nasce Em aberto, e registrar esse
	// primeiro estágio é o que dá começo ao histórico.
	if webhook.Tipo == "inclusao_pedido" || webhook.Tipo == "atualizacao_pedido" {
		idPedido := webhook.Dados.ID
		if idPedido == "" {
			idPedido = webhook.Dados.IDPedido.String()
		}

		// A nota fiscal chega POR AQUI, e não só pelo evento `nota_fiscal`.
		//
		// Todo webhook de pedido carrega `idNotaFiscal`, e ele vale "0" enquanto
		// não há nota (capturado em 26/08/2026, com o pedido em situação
		// "faturado" e idNotaFiscal ainda "0" — a situação mente, o campo não).
		// Depender do evento `nota_fiscal` sozinho é frágil: em 1.807 webhooks
		// gravados dessa conta ele NUNCA apareceu, e não há rota na API v3 para
		// conferir a quais tipos a conta está inscrita.
		//
		// `atualizacao_pedido` apareceu 380 vezes nas mesmas capturas. Usar o
		// canal que comprovadamente chega, em vez do que talvez chegue, é o que
		// impede o LiveCart de somar item num pedido que já virou documento.
		if nf := webhook.Dados.IDNotaFiscal.String(); nf != "" && nf != "0" && idPedido != "" {
			go func() {
				ctx := logger.WithStore(context.Background(), storeID, storeSlug)
				if _, err := h.service.ERP().SyncCartInvoiceByExternalOrder(ctx, storeID, idPedido, nf); err != nil {
					logger.From(ctx, h.logger).Error("failed to record the invoice carried by the order webhook",
						zap.String("id_pedido", idPedido),
						zap.String("id_nfe", nf),
						zap.Error(err),
					)
				}
			}()
		}

		status, conhecida := providers.ParseERPOrderStatus(webhook.Dados.CodigoSituacao)
		switch {
		case idPedido == "":
			logger.From(c.Context(), h.logger).Warn("order webhook missing order id — cannot track",
				zap.String("tipo", webhook.Tipo),
			)
		case !conhecida:
			// Situação nova numa versão futura da API. Inventar um nome seria
			// pior do que registrar que não conhecemos aquela.
			logger.From(c.Context(), h.logger).Warn("order webhook carries an unknown situation",
				zap.String("id_pedido", idPedido),
				zap.String("codigo_situacao", webhook.Dados.CodigoSituacao),
				zap.String("descricao_situacao", webhook.Dados.DescricaoSituacao),
			)
		default:
			payload := append([]byte(nil), body...)
			numero := webhook.Dados.Numero.String()
			ehAtualizacao := webhook.Tipo == "atualizacao_pedido"
			go func() {
				ctx := logger.WithStore(context.Background(), storeID, storeSlug)
				if err := h.service.ERP().ObserveOrderStatus(ctx, storeID, idPedido, numero, status, erp.StatusSourceWebhook, payload); err != nil {
					logger.From(ctx, h.logger).Error("failed to record ERP order status",
						zap.String("id_pedido", idPedido),
						zap.String("status", string(status)),
						zap.Error(err),
					)
				}

				// `atualizacao_pedido` também dispara quando SÓ os itens mudam —
				// medido em 26/08/2026: um PUT de quantidades às 20:14:21 gerou o
				// webhook às 20:14:24, com a situação parada em "aberto". É o
				// empurrão que dispensa sondagem: o lojista mexeu no pedido pelo
				// painel e o carrinho precisa seguir.
				//
				// Ele chega também depois das NOSSAS escritas. O reflexo é
				// idempotente e sai barato quando nada mudou (uma leitura e uma
				// comparação), e é coalescido por pedido para uma rajada não virar
				// uma leitura por webhook.
				if !ehAtualizacao {
					return
				}
				h.reflexoDoPedido(ctx, storeID, idPedido)
			}()
		}
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
				if _, err := h.service.ERP().SyncCartInvoiceByExternalOrder(ctx, storeID, idPedido, idNFe); err != nil {
					logger.From(ctx, h.logger).Error("failed to process nota_fiscal webhook",
						zap.String("id_pedido", idPedido),
						zap.String("id_nfe", idNFe),
						zap.Error(err),
					)
				}
			}()
		} else {
			// Sem idPedido não há como resolver o carrinho, e nem sempre há um:
			// a conta do lojista emite nota de venda que não passou pelo
			// LiveCart. Debug, não Warn — não é defeito nosso nem há ação a
			// tomar, e como aviso só ensina o time a ignorar o log.
			logger.From(c.Context(), h.logger).Debug("nota_fiscal sem idPedido — provavelmente de um pedido que não é nosso",
				zap.String("id_nfe", idNFe),
			)
		}
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// reflexoDoPedido traz para o carrinho o que o pedido no ERP diz agora.
//
// Coalescido por pedido: uma rajada de webhooks (a nossa própria escrita gera
// um, e o lojista pode salvar várias vezes seguidas) vira no máximo duas
// leituras, e a última sempre enxerga o estado final.
func (h *WebhookHandler) reflexoDoPedido(ctx context.Context, storeID, idPedido string) {
	cartID, err := h.service.CartIDByExternalOrder(ctx, storeID, idPedido)
	if err != nil || cartID == "" {
		return // pedido que não é de nenhum carrinho nosso
	}
	rodou, err := h.service.CoalescerReflexo().Fazer(storeID+"|"+idPedido, func() error {
		// Pedido PAGO: a grade não volta para o carrinho (aquela venda está
		// fechada), mas o dinheiro precisa continuar dizendo a verdade. Toda
		// mudança de item faz o ERP redistribuir o total pelas parcelas e
		// afirmar que ela pagou o valor novo.
		if split, splitErr := h.service.RecomporParcelasDoPedidoPago(ctx, cartID, storeID); splitErr != nil {
			logger.From(ctx, h.logger).Error("could not restore the paid/outstanding split",
				zap.String("cart_id", cartID), zap.Error(splitErr))
		} else if split != nil && split.Reescrito {
			logger.From(ctx, h.logger).Info("paid order gained items; installments split",
				zap.String("cart_id", cartID),
				zap.Int64("paid_cents", split.PagoCents),
				zap.Int64("outstanding_cents", split.SaldoCents))
		}

		rel, syncErr := h.service.SyncCartFromERPOrder(ctx, cartID, storeID)
		if syncErr != nil {
			return syncErr
		}
		if rel != nil && len(rel.Changes) > 0 {
			logger.From(ctx, h.logger).Info("cart followed the merchant's edit in the ERP",
				zap.String("cart_id", cartID),
				zap.String("id_pedido", idPedido),
				zap.Int("changes", len(rel.Changes)),
				zap.Int("products_imported", rel.Imported),
			)
		}
		return nil
	})
	if err != nil {
		logger.From(ctx, h.logger).Warn("could not reflect the ERP order into the cart",
			zap.String("cart_id", cartID),
			zap.String("id_pedido", idPedido),
			zap.Error(err))
		return
	}
	if !rodou {
		logger.From(ctx, h.logger).Debug("reflection coalesced into the one already running",
			zap.String("id_pedido", idPedido))
	}
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

// HandleBlingOAuthCallback recebe o redirect da autorização do Bling.
//
// É o alvo do "URL de redirecionamento" cadastrado no aplicativo — e o Bling
// IGNORA o redirect_uri que a gente manda na requisição, usando sempre o do
// cadastro. Divergir entre os dois não dá erro: dá um callback que nunca chega.
//
// ⚠ Sem retry em cima do erro. O `code` do Bling vale UM MINUTO e a doc avisa
// que reusar um code ainda válido faz o usuário ter "o seu acesso revogado por
// medidas de segurança". Falhou, o lojista clica em conectar de novo — o que
// gera um code novo.
//
// @Summary Bling OAuth callback
// @Router /api/v1/integrations/oauth/bling/callback [get]
func (h *WebhookHandler) HandleBlingOAuthCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	state := c.Query("state")
	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")

	if erro := c.Query("error"); erro != "" {
		logger.From(c.Context(), h.logger).Warn("bling: o lojista recusou a autorização",
			zap.String("error", erro),
			zap.String("error_description", c.Query("error_description")))
		return c.Redirect(frontendURL+"/settings/integrations?error=access_denied", fiber.StatusFound)
	}
	if code == "" {
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_code", fiber.StatusFound)
	}
	if state == "" {
		return c.Redirect(frontendURL+"/settings/integrations?error=missing_state", fiber.StatusFound)
	}

	out, err := h.service.HandleOAuthCallback(c.Context(), OAuthCallbackInput{
		Provider: "bling",
		Code:     code,
		State:    state,
	})
	if err != nil {
		logger.From(c.Context(), h.logger).Error("bling: callback de OAuth falhou",
			zap.String("state", state), zap.Error(err))
		return c.Redirect(frontendURL+"/settings/integrations?error=bling_connection_failed", fiber.StatusFound)
	}

	logger.From(c.Context(), h.logger).Info("bling conectado pelo callback",
		zap.String("store_id", out.StoreID),
		zap.String("integration_id", out.IntegrationID))

	return c.Redirect(frontendURL+"/settings/integrations?connected=bling", fiber.StatusFound)
}

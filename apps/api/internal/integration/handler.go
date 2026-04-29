package integration

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
)

// Handler handles HTTP requests for integrations.
type Handler struct {
	service  *Service
	validate *validator.Validate
}

// NewHandler creates a new integration handler.
func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{
		service:  service,
		validate: validate,
	}
}

// RegisterRoutes registers the integration routes.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/integrations")

	// CRUD
	g.Get("/", h.List)
	g.Post("/", h.Create)

	// Provider setup URLs (must be registered before /:id so it's not eaten by
	// the wildcard).
	g.Get("/providers/:provider/urls", h.GetProviderURLs)

	g.Get("/:id", h.GetByID)
	g.Delete("/:id", h.Delete)

	// Test connection
	g.Post("/:id/test", h.TestConnection)

	// Instagram operations
	g.Get("/instagram/lives", h.GetInstagramLives)

	// OAuth connect
	g.Get("/oauth/:provider/connect", h.OAuthConnect)

	// Shipping — token-based connect (no OAuth) + order lifecycle helpers.
	// These are typed at the provider level because each shipping provider
	// has its own auth model; routing by :provider keeps a single surface.
	g.Post("/shipping/smartenvios/connect", h.ConnectSmartEnvios)
	g.Get("/shipping/:provider/carriers", h.ListShippingCarriers)
	g.Post("/shipping/:provider/shipments", h.CreateShippingShipment)
	g.Post("/shipping/:provider/shipments/:shipmentId/invoice", h.AttachShippingInvoice)
	g.Post("/shipping/:provider/shipments/:shipmentId/invoice-xml", h.UploadShippingInvoiceXML)
	g.Post("/shipping/:provider/labels", h.GenerateShippingLabels)
	g.Post("/shipping/:provider/tracking", h.TrackShipping)

	// ERP operations
	g.Get("/:id/products", h.SearchProducts)
	g.Post("/:id/products/:productId/sync", h.SyncProduct)
	g.Post("/:id/products/:tinyProductId/import", h.ImportERPProduct)

	// Payment operations (Mercado Pago)
	g.Post("/:id/checkout", h.CreateCheckout)
	g.Get("/:id/payments/:paymentId", h.GetPaymentStatus)
	g.Post("/:id/payments/:paymentId/refund", h.RefundPayment)
}

// =============================================================================
// CRUD HANDLERS
// =============================================================================

// Create creates a new integration.
// @Summary Create integration
// @Description Creates a new external service integration
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body CreateIntegrationRequest true "Integration data"
// @Success 201 {object} httpx.Envelope{data=IntegrationResponse}
// @Failure 400 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/integrations [post]
// @Security BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req CreateIntegrationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Convert credentials map to Credentials struct
	creds := mapToCredentials(req.Credentials)

	output, err := h.service.Create(c.Context(), CreateIntegrationInput{
		StoreID:     storeID,
		Type:        req.Type,
		Provider:    req.Provider,
		Credentials: creds,
		Metadata:    req.Metadata,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, toIntegrationResponse(output))
}

// List lists all integrations for a store.
// @Summary List integrations
// @Description Lists all integrations for the current store
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Success 200 {object} httpx.Envelope{data=ListIntegrationsResponse}
// @Router /api/v1/stores/{storeId}/integrations [get]
// @Security BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	input := ListIntegrationsInput{
		StoreID: storeID,
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
	}

	output, err := h.service.List(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	integrations := make([]IntegrationResponse, len(output.Integrations))
	for i, o := range output.Integrations {
		integrations[i] = *toIntegrationResponse(&o)
	}

	return httpx.OK(c, ListIntegrationsResponse{
		Data:       integrations,
		Pagination: query.NewPaginationResponse(output.Pagination, output.Total),
	})
}

// GetByID retrieves an integration by ID.
// @Summary Get integration
// @Description Retrieves an integration by ID
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=IntegrationResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id} [get]
// @Security BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toIntegrationResponse(output))
}

// Delete deletes an integration.
// @Summary Delete integration
// @Description Deletes an integration
// @Tags integrations
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id} [delete]
// @Security BearerAuth
func (h *Handler) Delete(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	if err := h.service.Delete(c.Context(), id, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, id)
}

// TestConnection tests the connection to the integration provider.
// @Summary Test integration connection
// @Description Tests if the integration credentials are valid and the provider is reachable
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=TestConnectionResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/test [post]
// @Security BearerAuth
func (h *Handler) TestConnection(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.TestConnection(c.Context(), TestConnectionInput{
		StoreID:       storeID,
		IntegrationID: id,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, TestConnectionResponse{
		Success:     output.Success,
		Message:     output.Message,
		LatencyMs:   output.Latency.Milliseconds(),
		AccountInfo: output.AccountInfo,
		TestedAt:    output.TestedAt,
	})
}

// =============================================================================
// INSTAGRAM HANDLERS
// =============================================================================

// GetInstagramLives retrieves all active Instagram lives for the store.
// @Summary Get active Instagram lives
// @Description Returns all live videos currently being broadcast on the connected Instagram account
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope{data=InstagramLivesResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/lives [get]
// @Security BearerAuth
func (h *Handler) GetInstagramLives(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	lives, err := h.service.FetchInstagramLives(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, map[string]any{"data": lives})
}

// =============================================================================
// ERP HANDLERS
// =============================================================================

// SearchProducts searches for products in an ERP integration.
// @Summary Search ERP products
// @Description Searches for products in an ERP integration by name, SKU, or barcode
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param search query string true "Search term (product name, SKU, or barcode)"
// @Param limit query int false "Max results" default(20)
// @Success 200 {object} httpx.Envelope{data=SearchProductsOutput}
// @Failure 400 {object} httpx.Envelope
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/products [get]
// @Security BearerAuth
func (h *Handler) SearchProducts(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	search := c.Query("search")

	if search == "" {
		return httpx.BadRequest(c, "search parameter is required")
	}

	output, err := h.service.SearchProducts(c.Context(), SearchProductsInput{
		StoreID:       storeID,
		IntegrationID: id,
		Search:        search,
		PageSize:      c.QueryInt("limit", 20),
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, output)
}

// ImportERPProduct imports a product (or a subset of its variations) from the
// ERP into the LiveCart catalog. If the product has no variations, a single
// simple product is created. If it has variations, a product_group with N
// variants is created in one transaction.
//
// Body:
//   { "variantIds": ["67890", "67891"] }   // optional subset
//   { }                                    // import everything
//
// @Summary Import product from ERP into LiveCart catalog
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param tinyProductId path string true "ERP product ID (Tiny parent or simple)"
// @Param request body ImportERPProductRequest false "Optional subset of variant IDs"
// @Success 201 {object} httpx.Envelope{data=ImportERPProductOutput}
// @Failure 404 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/products/{tinyProductId}/import [post]
// @Security BearerAuth
func (h *Handler) ImportERPProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")
	tinyProductID := c.Params("tinyProductId")

	var req ImportERPProductRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return httpx.BadRequest(c, "invalid request body")
		}
		if err := h.validate.Struct(req); err != nil {
			return httpx.ValidationError(c, err)
		}
	}

	output, err := h.service.ImportERPProduct(c.Context(), ImportERPProductInput{
		StoreID:       storeID,
		IntegrationID: integrationID,
		TinyProductID: tinyProductID,
		VariantIDs:    req.VariantIDs,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Created(c, output)
}

// SyncProduct manually syncs a product from the ERP to LiveCart.
// @Summary Sync product from ERP
// @Description Fetches the latest product data from the ERP and updates the local product
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param productId path string true "Product ID (LiveCart)"
// @Success 200 {object} httpx.Envelope{data=SyncProductOutput}
// @Failure 404 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/products/{productId}/sync [post]
// @Security BearerAuth
func (h *Handler) SyncProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")
	productID := c.Params("productId")

	output, err := h.service.SyncProductManual(c.Context(), SyncProductInput{
		StoreID:       storeID,
		IntegrationID: integrationID,
		ProductID:     productID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, output)
}

// =============================================================================
// PAYMENT HANDLERS
// =============================================================================

// CreateCheckout creates a payment checkout session.
// @Summary Create checkout
// @Description Creates a payment checkout session
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param X-Idempotency-Key header string false "Idempotency key"
// @Param body body CreateCheckoutRequest true "Checkout data"
// @Success 201 {object} httpx.Envelope{data=CheckoutResponse}
// @Failure 400 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/checkout [post]
// @Security BearerAuth
func (h *Handler) CreateCheckout(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	idempotencyKey := c.Get("X-Idempotency-Key")

	var req CreateCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	// Override integration ID from path
	req.IntegrationID = id

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.CreateCheckout(c.Context(), CreateCheckoutInput{
		StoreID:        storeID,
		IntegrationID:  id,
		IdempotencyKey: idempotencyKey,
		CartID:         req.CartID,
		Items:          req.Items,
		Customer:       req.Customer,
		TotalAmount:    req.TotalAmount,
		Currency:       req.Currency,
		SuccessURL:     req.SuccessURL,
		FailureURL:     req.FailureURL,
		Metadata:       req.Metadata,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, CheckoutResponse{
		CheckoutID:  output.CheckoutID,
		CheckoutURL: output.CheckoutURL,
		ExpiresAt:   output.ExpiresAt,
	})
}

// GetPaymentStatus retrieves the status of a payment.
// @Summary Get payment status
// @Description Retrieves the current status of a payment
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param paymentId path string true "Payment ID"
// @Success 200 {object} httpx.Envelope{data=PaymentStatusResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/payments/{paymentId} [get]
// @Security BearerAuth
func (h *Handler) GetPaymentStatus(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	paymentID := c.Params("paymentId")

	output, err := h.service.GetPaymentStatus(c.Context(), GetPaymentStatusInput{
		StoreID:       storeID,
		IntegrationID: id,
		PaymentID:     paymentID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, PaymentStatusResponse{
		PaymentID:     output.PaymentID,
		Status:        output.Status,
		Amount:        output.Amount,
		PaidAt:        output.PaidAt,
		RefundedAt:    output.RefundedAt,
		FailureReason: output.FailureReason,
		Metadata:      output.Metadata,
	})
}

// RefundPayment initiates a refund for a payment.
// @Summary Refund payment
// @Description Initiates a refund for a payment
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param paymentId path string true "Payment ID"
// @Param body body RefundRequest true "Refund data"
// @Success 200 {object} httpx.Envelope{data=RefundResponse}
// @Failure 400 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/payments/{paymentId}/refund [post]
// @Security BearerAuth
func (h *Handler) RefundPayment(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	paymentID := c.Params("paymentId")

	var req RefundRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	output, err := h.service.RefundPayment(c.Context(), RefundPaymentInput{
		StoreID:       storeID,
		IntegrationID: id,
		PaymentID:     paymentID,
		Amount:        req.Amount,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, RefundResponse{
		RefundID:  output.RefundID,
		Status:    output.Status,
		Amount:    output.Amount,
		CreatedAt: output.CreatedAt,
	})
}

// =============================================================================
// OAUTH HANDLERS
// =============================================================================

// OAuthConnect initiates the OAuth flow for a provider.
// @Summary Start OAuth connection
// @Description Returns the OAuth authorization URL for the provider
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param provider path string true "Provider name (mercado_pago, tiny)"
// @Success 200 {object} httpx.Envelope{data=OAuthConnectResponse}
// @Failure 400 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/oauth/{provider}/connect [get]
// @Security BearerAuth
func (h *Handler) OAuthConnect(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	provider := c.Params("provider")

	output, err := h.service.GetOAuthURL(c.Context(), GetOAuthURLInput{
		StoreID:  storeID,
		Provider: provider,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, OAuthConnectResponse{
		AuthURL: output.AuthURL,
		State:   output.State,
	})
}

// GetProviderURLs returns the redirect (OAuth callback) and webhook URLs the
// merchant must paste into the provider's app config (e.g. Tiny).
// @Summary Get provider setup URLs
// @Description Returns the redirect URL and webhook URL the merchant must paste into the provider's app config
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param provider path string true "Provider name (e.g. tiny, mercado_pago, pagarme)"
// @Success 200 {object} httpx.Envelope{data=ProviderURLsResponse}
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/providers/{provider}/urls [get]
// @Security BearerAuth
func (h *Handler) GetProviderURLs(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	provider := c.Params("provider")

	output, err := h.service.GetProviderURLs(c.Context(), GetProviderURLsInput{
		StoreID:  storeID,
		Provider: provider,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, ProviderURLsResponse{
		Provider:    output.Provider,
		RedirectURL: output.RedirectURL,
		WebhookURL:  output.WebhookURL,
	})
}

// =============================================================================
// HELPERS
// =============================================================================

func mapToCredentials(m map[string]any) *providers.Credentials {
	if m == nil {
		return nil
	}

	creds := &providers.Credentials{
		Extra: make(map[string]any),
	}

	if v, ok := m["access_token"].(string); ok {
		creds.AccessToken = v
	}
	if v, ok := m["refresh_token"].(string); ok {
		creds.RefreshToken = v
	}
	if v, ok := m["token_type"].(string); ok {
		creds.TokenType = v
	}
	if v, ok := m["api_key"].(string); ok {
		creds.APIKey = v
	}
	if v, ok := m["api_secret"].(string); ok {
		creds.APISecret = v
	}

	// Copy remaining fields to Extra
	for k, v := range m {
		switch k {
		case "access_token", "refresh_token", "token_type", "api_key", "api_secret", "expires_at":
			continue
		default:
			creds.Extra[k] = v
		}
	}

	return creds
}

func toIntegrationResponse(output *CreateIntegrationOutput) *IntegrationResponse {
	return &IntegrationResponse{
		ID:                output.ID,
		StoreID:           output.StoreID,
		Type:              output.Type,
		Provider:          output.Provider,
		Status:            output.Status,
		Metadata:          output.Metadata,
		LastSyncedAt:      output.LastSyncedAt,
		CreatedAt:         output.CreatedAt,
		RedirectURL:       output.RedirectURL,
		WebhookURL:        output.WebhookURL,
		WebhookLastPingAt: output.WebhookLastPingAt,
	}
}

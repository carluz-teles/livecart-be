package checkout

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// Handler handles HTTP requests for public checkout.
type Handler struct {
	service  *Service
	validate *validator.Validate
}

// NewHandler creates a new checkout handler.
func NewHandler(service *Service) *Handler {
	return &Handler{
		service:  service,
		validate: validator.New(),
	}
}

// RegisterRoutes registers public checkout routes.
func (h *Handler) RegisterRoutes(app *fiber.App) {
	// Public routes - no authentication required
	checkout := app.Group("/api/public/checkout")
	checkout.Get("/:token", h.GetCartForCheckout)
	checkout.Post("/:token", h.GenerateCheckout)

	// Transparent checkout routes
	checkout.Get("/:token/config", h.GetCheckoutConfig)
	checkout.Post("/:token/card", h.ProcessCardPayment)
	checkout.Post("/:token/pix", h.GeneratePix)
	checkout.Get("/:token/status", h.GetPaymentStatus)
}

// GetCartForCheckout handles GET /api/public/checkout/:token
// Returns cart details for the checkout page.
func (h *Handler) GetCartForCheckout(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	output, err := h.service.GetCartForCheckout(c.Context(), GetCartForCheckoutInput{
		Token: token,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, h.toCartResponse(output))
}

// GenerateCheckout handles POST /api/public/checkout/:token
// Generates a payment link and returns the checkout URL.
func (h *Handler) GenerateCheckout(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req GenerateCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.GenerateCheckout(c.Context(), GenerateCheckoutInput{
		Token: token,
		Email: req.Email,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, GenerateCheckoutResponse{
		CheckoutURL: output.CheckoutURL,
		ExpiresAt:   output.ExpiresAt,
	})
}

// =============================================================================
// TRANSPARENT CHECKOUT HANDLERS
// =============================================================================

// GetCheckoutConfig handles GET /api/public/checkout/:token/config
// Returns checkout configuration including public key and available methods.
func (h *Handler) GetCheckoutConfig(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	output, err := h.service.GetCheckoutConfig(c.Context(), GetCheckoutConfigInput{
		Token: token,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, GetCheckoutConfigResponse{
		Provider:         output.Provider,
		PublicKey:        output.PublicKey,
		AvailableMethods: output.AvailableMethods,
		TotalAmount:      output.TotalAmount,
		Currency:         output.Currency,
	})
}

// ProcessCardPayment handles POST /api/public/checkout/:token/card
// Processes a card payment using a tokenized card.
func (h *Handler) ProcessCardPayment(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req ProcessCardPaymentRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.ProcessCardPayment(c.Context(), ProcessCardPaymentInput{
		Token:            token,
		Email:            req.Email,
		CardToken:        req.Token,
		Installments:     req.Installments,
		PaymentMethodID:  req.PaymentMethodID,
		IssuerID:         req.IssuerID,
		DeviceID:         req.DeviceID,
		CustomerName:     req.CustomerName,
		CustomerDocument: req.CustomerDocument,
		CustomerPhone:    req.CustomerPhone,
		ShippingAddress:  req.ShippingAddress,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, ProcessCardPaymentResponse{
		PaymentID:      output.PaymentID,
		Status:         output.Status,
		StatusDetail:   output.StatusDetail,
		Message:        output.Message,
		Amount:         output.Amount,
		Installments:   output.Installments,
		LastFourDigits: output.LastFourDigits,
		CardBrand:      output.CardBrand,
	})
}

// GeneratePix handles POST /api/public/checkout/:token/pix
// Generates a PIX QR code for payment.
func (h *Handler) GeneratePix(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req GeneratePixRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.GeneratePix(c.Context(), GeneratePixInput{
		Token:            token,
		Email:            req.Email,
		CustomerName:     req.CustomerName,
		CustomerDocument: req.CustomerDocument,
		CustomerPhone:    req.CustomerPhone,
		ShippingAddress:  req.ShippingAddress,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, GeneratePixResponse{
		PaymentID:  output.PaymentID,
		QRCode:     output.QRCode,
		QRCodeText: output.QRCodeText,
		Amount:     output.Amount,
		ExpiresAt:  output.ExpiresAt,
		TicketURL:  output.TicketURL,
	})
}

// GetPaymentStatus handles GET /api/public/checkout/:token/status
// Returns the current payment status for polling.
func (h *Handler) GetPaymentStatus(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	output, err := h.service.GetPaymentStatus(c.Context(), GetPaymentStatusInput{
		Token: token,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, GetPaymentStatusResponse{
		Status:        output.Status,
		PaymentStatus: output.PaymentStatus,
		PaidAt:        output.PaidAt,
		Message:       output.Message,
	})
}

// =============================================================================
// RESPONSE BUILDERS
// =============================================================================

func (h *Handler) toCartResponse(output *GetCartForCheckoutOutput) CartForCheckoutResponse {
	items := make([]CartItemResponse, len(output.Items))
	var subtotal int64
	var totalItems int

	for i, item := range output.Items {
		// Calculate available quantity (total - waitlisted)
		availableQty := item.Quantity - item.WaitlistedQuantity

		items[i] = CartItemResponse{
			ID:                 item.ID,
			ProductID:          item.ProductID,
			Name:               item.Name,
			ImageURL:           item.ImageURL,
			Keyword:            item.Keyword,
			Quantity:           item.Quantity,
			UnitPrice:          item.UnitPrice,
			TotalPrice:         item.UnitPrice * int64(item.Quantity),
			WaitlistedQuantity: item.WaitlistedQuantity,
		}

		// Only count available (non-waitlisted) items in totals
		if availableQty > 0 {
			subtotal += item.UnitPrice * int64(availableQty)
			totalItems += availableQty
		}
	}

	return CartForCheckoutResponse{
		ID:                 output.Cart.ID,
		Token:              output.Cart.Token,
		Status:             output.Cart.Status,
		CustomerEmail:      output.Cart.CustomerEmail,
		PaymentStatus:      output.Cart.PaymentStatus,
		CheckoutURL:        output.Cart.CheckoutURL,
		PlatformHandle:     output.Cart.PlatformHandle,
		AllowEdit:          output.Cart.AllowEdit,
		MaxQuantityPerItem: output.Cart.MaxQuantityPerItem,
		ExpiresAt:          output.Cart.ExpiresAt,
		CreatedAt:          output.Cart.CreatedAt,
		Event: CartEventInfo{
			ID:    output.Cart.EventID,
			Title: output.Cart.EventTitle,
		},
		Store: CartStoreInfo{
			ID:      output.Cart.StoreID,
			Name:    output.Cart.StoreName,
			LogoURL: output.Cart.StoreLogoURL,
		},
		Items: items,
		Summary: CartSummary{
			Subtotal:   subtotal,
			TotalItems: totalItems,
		},
	}
}

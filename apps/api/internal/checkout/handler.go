package checkout

import (
	"context"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/storage"
)

// Handler handles HTTP requests for public checkout.
type Handler struct {
	service  *Service
	validate *validator.Validate
	s3Client *storage.S3Client
}

// NewHandler creates a new checkout handler.
func NewHandler(service *Service, s3Client *storage.S3Client) *Handler {
	return &Handler{
		service:  service,
		validate: validator.New(),
		s3Client: s3Client,
	}
}

// RegisterRoutes registers public checkout routes.
func (h *Handler) RegisterRoutes(app *fiber.App) {
	// Public routes - no authentication required
	checkout := app.Group("/api/public/checkout")
	checkout.Get("/:token", h.GetCartForCheckout)
	checkout.Post("/:token", h.GenerateCheckout)

	// Shipping routes
	checkout.Post("/:token/shipping-quote", h.QuoteShipping)
	checkout.Put("/:token/shipping-method", h.SelectShippingMethod)

	// Transparent checkout routes
	checkout.Get("/:token/config", h.GetCheckoutConfig)
	checkout.Post("/:token/card", h.ProcessCardPayment)
	checkout.Post("/:token/pix", h.GeneratePix)
	checkout.Get("/:token/status", h.GetPaymentStatus)
}

// QuoteShipping handles POST /api/public/checkout/:token/shipping-quote
// Returns the carrier options available for the cart destination zip.
func (h *Handler) QuoteShipping(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req ShippingQuoteRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	out, err := h.service.QuoteShipping(c.Context(), QuoteShippingInput{
		Token:   token,
		ZipCode: req.ZipCode,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, ShippingQuoteResponse{
		QuotedAt:     out.QuotedAt,
		FreeShipping: out.FreeShipping,
		Options:      out.Options,
	})
}

// SelectShippingMethod handles PUT /api/public/checkout/:token/shipping-method
// Re-quotes the chosen service and persists the selection on the cart.
func (h *Handler) SelectShippingMethod(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req SelectShippingMethodRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	out, err := h.service.SelectShippingMethod(c.Context(), SelectShippingMethodInput{
		Token:     token,
		ServiceID: req.ServiceID,
		ZipCode:   req.ZipCode,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, SelectShippingMethodResponse{
		Shipping: out.Shipping,
		Summary:  out.Summary,
	})
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
		PaymentID:         output.PaymentID,
		Status:            output.Status,
		StatusDetail:      output.StatusDetail,
		Message:           output.Message,
		Amount:            output.Amount,
		Installments:      output.Installments,
		LastFourDigits:    output.LastFourDigits,
		CardBrand:         output.CardBrand,
		AuthorizationCode: output.AuthorizationCode,
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

	summary := CartSummary{
		Subtotal:   subtotal,
		TotalItems: totalItems,
		Total:      subtotal,
	}
	if output.Cart.Shipping != nil {
		summary.ShippingCost = output.Cart.Shipping.CostCents
		summary.Total = subtotal + output.Cart.Shipping.CostCents
		summary.HasShippingQuote = true
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
		PaidAt:             output.Cart.PaidAt,
		CreatedAt:          output.Cart.CreatedAt,
		Event: CartEventInfo{
			ID:    output.Cart.EventID,
			Title: output.Cart.EventTitle,
		},
		Store: CartStoreInfo{
			ID:      output.Cart.StoreID,
			Name:    output.Cart.StoreName,
			LogoURL: h.getPresignedLogoURL(output.Cart.StoreLogoURL),
		},
		Items:           items,
		Summary:         summary,
		Shipping:        output.Cart.Shipping,
		Customer:        toCheckoutCustomer(output.Customer),
		ShippingAddress: toCheckoutShippingAddress(output.ShippingAddress),
		Payment:         toCheckoutPayment(output.Payment, output.Cart.PaidAt, output.Cart.CheckoutID),
	}
}

// toCheckoutCustomer projects the service-level customer info onto the public
// response shape. Returns nil when the service did not load it (i.e. the cart
// is not paid, or the buyer fields are all empty).
func toCheckoutCustomer(c *CartCustomerInfo) *CheckoutCustomerInfo {
	if c == nil {
		return nil
	}
	return &CheckoutCustomerInfo{
		Name:     c.Name,
		Document: c.Document,
		Phone:    c.Phone,
		Email:    c.Email,
	}
}

func toCheckoutShippingAddress(a *CartShippingAddressInfo) *CheckoutShippingAddressInfo {
	if a == nil {
		return nil
	}
	return &CheckoutShippingAddressInfo{
		ZipCode:      a.ZipCode,
		Street:       a.Street,
		Number:       a.Number,
		Complement:   a.Complement,
		Neighborhood: a.Neighborhood,
		City:         a.City,
		State:        a.State,
	}
}

// toCheckoutPayment projects the persisted payment metadata onto the public
// response shape. Method is normalized to the public values "pix" | "card";
// unknown raw values fall back to "card" so the receipt page always has a
// usable label. Returns nil when there is no method recorded at all
// (e.g. paid carts from before payment_method was tracked).
//
// paymentID is the gateway transaction identifier — the cart row stores it
// in `checkout_id` after payment confirmation; we surface it as `paymentId`.
func toCheckoutPayment(p *CartPaymentInfo, paidAt *time.Time, paymentID *string) *CheckoutPaymentInfo {
	if p == nil || paidAt == nil {
		return nil
	}
	method := normalizePaymentMethod(p.RawMethod)
	if method == "" {
		return nil
	}
	info := &CheckoutPaymentInfo{
		Method: method,
		PaidAt: *paidAt,
	}
	if paymentID != nil {
		info.PaymentID = *paymentID
	}
	if method == "card" {
		info.Installments = p.Installments
		info.CardBrand = p.CardBrand
		info.LastFourDigits = p.LastFourDigits
		info.AuthorizationCode = p.AuthorizationCode
	}
	return info
}

// normalizePaymentMethod maps the persisted carts.payment_method value (set
// by the providers — "pix", "credit_card", "debit_card", "boleto", ...) to
// the public-facing "pix" | "card" enum used by the comprovante page.
// Returns "" when there's nothing recorded so the caller can omit the block.
func normalizePaymentMethod(raw string) string {
	switch raw {
	case "":
		return ""
	case "pix":
		return "pix"
	default:
		// credit_card / debit_card / and any future card-like value.
		return "card"
	}
}

// getPresignedLogoURL generates a presigned URL for the store logo if available.
func (h *Handler) getPresignedLogoURL(logoURL *string) *string {
	if h.s3Client == nil || logoURL == nil || *logoURL == "" {
		return nil
	}
	presignedURL, err := h.s3Client.GeneratePresignedGetURL(context.Background(), *logoURL, 0)
	if err != nil {
		return nil
	}
	return &presignedURL
}

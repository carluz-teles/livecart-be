package checkout

import (
	"context"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/storage"
)

// clientIP returns the buyer's real client IP for antifraud enrichment. The
// app runs behind a proxy and Fiber is not configured with a trusted proxy, so
// c.IP() yields the proxy hop, not the buyer. X-Forwarded-For is
// "client, proxy1, proxy2, ..." — the leftmost entry is the origin client.
// Falls back to c.IP() when the header is absent (e.g. local requests).
func clientIP(c *fiber.Ctx) string {
	if xff := c.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
			return first
		}
	}
	return c.IP()
}

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

	// Buyer cart-edit routes (PATCH/DELETE/POST items). Protected the same way
	// as the rest of the public checkout: by knowledge of the cart token and by
	// the cart's allow_edit + payment_status state — see Service.loadEditableCart.
	checkout.Post("/:token/items", h.AddCartItem)
	checkout.Patch("/:token/items/:itemId", h.UpdateCartItemQuantity)
	checkout.Delete("/:token/items/:itemId", h.RemoveCartItem)

	// "Sair da fila": cliente desiste de uma waitlist entry. Ownership é
	// validada na query (cart_id no WHERE) — basta o token do cart.
	checkout.Delete("/:token/waitlist/:id", h.DropFromWaitlist)
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
		QuotedAt:            out.QuotedAt,
		FreeShipping:        out.FreeShipping,
		Options:             out.Options,
		PickupAddress:       out.PickupAddress,
		NoShippingAvailable: out.NoShippingAvailable,
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
		MaxInstallments:  output.MaxInstallments,
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
		IPAddress:        clientIP(c),
		CustomerName:     req.CustomerName,
		CustomerDocument: req.CustomerDocument,
		CustomerPhone:    req.CustomerPhone,
		WhatsappConsent:  req.WhatsappConsent,
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
		WhatsappConsent:  req.WhatsappConsent,
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
// CART ITEM MUTATIONS
// =============================================================================

// UpdateCartItemQuantity handles PATCH /api/public/checkout/:token/items/:itemId
func (h *Handler) UpdateCartItemQuantity(c *fiber.Ctx) error {
	token := c.Params("token")
	itemID := c.Params("itemId")
	if token == "" || itemID == "" {
		return httpx.BadRequest(c, "token and itemId are required")
	}

	var req UpdateCartItemQuantityRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.UpdateCartItemQuantity(c.Context(), MutateCartItemInput{
		Token:    token,
		ItemID:   itemID,
		Quantity: req.Quantity,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, h.toCartResponse(output))
}

// RemoveCartItem handles DELETE /api/public/checkout/:token/items/:itemId
func (h *Handler) RemoveCartItem(c *fiber.Ctx) error {
	token := c.Params("token")
	itemID := c.Params("itemId")
	if token == "" || itemID == "" {
		return httpx.BadRequest(c, "token and itemId are required")
	}

	output, err := h.service.RemoveCartItem(c.Context(), MutateCartItemInput{
		Token:  token,
		ItemID: itemID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, h.toCartResponse(output))
}

// DropFromWaitlist handles DELETE /api/public/checkout/:token/waitlist/:id —
// o cliente desiste de continuar na fila por um produto. Se a entry estava
// 'notified' (já reservada para ele), o stock volta para o próximo da fila;
// 'waiting' apenas marca como cancelled.
func (h *Handler) DropFromWaitlist(c *fiber.Ctx) error {
	token := c.Params("token")
	id := c.Params("id")
	if token == "" || id == "" {
		return httpx.BadRequest(c, "token and id are required")
	}

	output, err := h.service.DropFromWaitlist(c.Context(), DropFromWaitlistInput{
		Token:          token,
		WaitlistItemID: id,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, h.toCartResponse(output))
}

// AddCartItem handles POST /api/public/checkout/:token/items
func (h *Handler) AddCartItem(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req AddCartItemRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.AddCartItem(c.Context(), MutateCartItemInput{
		Token:     token,
		ProductID: req.ProductID,
		Quantity:  req.Quantity,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, h.toCartResponse(output))
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

		// Product images live in a private bucket; serve a fresh presigned URL
		// (external ERP URLs pass through unchanged).
		imageURL := item.ImageURL
		if h.s3Client != nil && imageURL != nil {
			presigned := h.s3Client.PresignImageURL(context.Background(), *imageURL)
			imageURL = &presigned
		}

		items[i] = CartItemResponse{
			ID:                 item.ID,
			ProductID:          item.ProductID,
			Name:               item.Name,
			ImageURL:           imageURL,
			Keyword:            item.Keyword,
			Quantity:           item.Quantity,
			UnitPrice:          item.UnitPrice,
			TotalPrice:         item.UnitPrice * int64(item.Quantity),
			WaitlistedQuantity: item.WaitlistedQuantity,
			AvailableStock:     item.AvailableStock,
			GroupName:          item.GroupName,
			Variant:            item.Variant,
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
	if output.Cart.CouponDiscountCents > 0 {
		summary.CouponDiscount = output.Cart.CouponDiscountCents
		summary.Total -= output.Cart.CouponDiscountCents
		if summary.Total < 0 {
			summary.Total = 0
		}
	}

	// Pix discount preview — applied off (subtotal − couponDiscount). The
	// `total` field above stays unchanged because the discount only kicks
	// in when the buyer actually picks Pix at checkout. Service.GeneratePix
	// applies the same percentage on the gateway-charged amount.
	summary.PixDiscountPercent = output.Cart.EventPixDiscountPercent
	if summary.PixDiscountPercent > 0 {
		base := subtotal - output.Cart.CouponDiscountCents
		if base < 0 {
			base = 0
		}
		summary.PixDiscountCents = base * int64(summary.PixDiscountPercent) / 100
	}

	var appliedCoupon *AppliedCoupon
	if output.Cart.CouponCode != nil {
		appliedCoupon = &AppliedCoupon{
			Code:             *output.Cart.CouponCode,
			Type:             output.Cart.CouponType,
			DiscountCents:    output.Cart.CouponDiscountCents,
			MaxDiscountCents: output.Cart.CouponMaxDiscountCents,
			MinPurchaseCents: output.Cart.CouponMinPurchaseCents,
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
		PaidAt:             output.Cart.PaidAt,
		CreatedAt:          output.Cart.CreatedAt,
		Event: CartEventInfo{
			ID:                 output.Cart.EventID,
			Title:              output.Cart.EventTitle,
			Type:               output.Cart.EventType,
			PixDiscountPercent: output.Cart.EventPixDiscountPercent,
			FreeShipping:       output.Cart.EventFreeShipping,
		},
		Store: CartStoreInfo{
			ID:      output.Cart.StoreID,
			Name:    output.Cart.StoreName,
			LogoURL: h.getPresignedLogoURL(output.Cart.StoreLogoURL),
		},
		Items:               items,
		WaitlistItems:       toWaitlistItemResponses(output.WaitlistItems),
		Summary:             summary,
		Shipping:            output.Cart.Shipping,
		Customer:            toCheckoutCustomer(output.Customer),
		ShippingAddress:     toCheckoutShippingAddress(output.ShippingAddress),
		Payment:             toCheckoutPayment(output.Payment, output.Cart.PaidAt, output.Cart.CheckoutID),
		IsReturningCustomer: output.Cart.IsReturningCustomer,
		AppliedCoupon:       appliedCoupon,
	}
}

// toWaitlistItemResponses preserves the service-level shape on the wire and
// guarantees a non-nil slice (the FE iterates without a null-guard).
func toWaitlistItemResponses(items []WaitlistItemDetails) []WaitlistItemResponse {
	out := make([]WaitlistItemResponse, len(items))
	for i, w := range items {
		out[i] = WaitlistItemResponse{
			ID:           w.ID,
			ProductID:    w.ProductID,
			ProductName:  w.ProductName,
			ProductImage: w.ProductImage,
			UnitPrice:    w.UnitPrice,
			Quantity:     w.Quantity,
			Position:     w.Position,
			Status:       w.Status,
			NotifiedAt:   w.NotifiedAt,
			ExpiresAt:    w.ExpiresAt,
			CreatedAt:    w.CreatedAt,
		}
	}
	return out
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

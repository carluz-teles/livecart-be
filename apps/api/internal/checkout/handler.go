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
// RESPONSE BUILDERS
// =============================================================================

func (h *Handler) toCartResponse(output *GetCartForCheckoutOutput) CartForCheckoutResponse {
	items := make([]CartItemResponse, len(output.Items))
	var subtotal int64
	var totalItems int

	for i, item := range output.Items {
		items[i] = CartItemResponse{
			ID:         item.ID,
			ProductID:  item.ProductID,
			Name:       item.Name,
			ImageURL:   item.ImageURL,
			Keyword:    item.Keyword,
			Quantity:   item.Quantity,
			UnitPrice:  item.UnitPrice,
			TotalPrice: item.UnitPrice * int64(item.Quantity),
			Waitlisted: item.Waitlisted,
		}

		if !item.Waitlisted {
			subtotal += item.UnitPrice * int64(item.Quantity)
			totalItems += item.Quantity
		}
	}

	return CartForCheckoutResponse{
		ID:             output.Cart.ID,
		Token:          output.Cart.Token,
		Status:         output.Cart.Status,
		CustomerEmail:  output.Cart.CustomerEmail,
		PaymentStatus:  output.Cart.PaymentStatus,
		CheckoutURL:    output.Cart.CheckoutURL,
		PlatformHandle: output.Cart.PlatformHandle,
		AllowEdit:      output.Cart.AllowEdit,
		ExpiresAt:      output.Cart.ExpiresAt,
		CreatedAt:      output.Cart.CreatedAt,
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

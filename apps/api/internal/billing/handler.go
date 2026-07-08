package billing

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// Handler exposes the store-scoped billing endpoints. The AccessGuard
// allowlists everything under /billing so a blocked merchant can still pay.
type Handler struct {
	service  *Service
	validate *validator.Validate
}

// NewHandler builds the billing handler.
func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

// RegisterRoutes mounts /billing under the store-scoped group.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/billing")
	g.Get("/subscription", h.GetSubscription)
	g.Post("/checkout", h.CreateCheckout)
}

// GetSubscription returns the paywall/subscription snapshot for the store.
// @Summary Get subscription state
// @Description Returns plan, status, trial days left and blocked flag
// @Tags billing
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope{data=SubscriptionState}
// @Router /api/v1/stores/{storeId}/billing/subscription [get]
// @Security BearerAuth
func (h *Handler) GetSubscription(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	state, err := h.service.GetState(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if state == nil {
		// Legacy store — provision the trial lazily and return it.
		state, err = h.service.EnsureTrialSubscription(c.Context(), storeID, "", "")
		if err != nil {
			return httpx.HandleServiceError(c, err)
		}
	}
	return httpx.OK(c, state)
}

// CreateCheckoutRequest picks the plan being contracted.
type CreateCheckoutRequest struct {
	Plan string `json:"plan" validate:"required,oneof=start grow scale"`
}

// CreateCheckout opens the hosted Stripe Checkout for conversion.
// @Summary Create conversion checkout
// @Description Returns the hosted Checkout URL that collects the card for the chosen plan
// @Tags billing
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body CreateCheckoutRequest true "Chosen plan"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/billing/checkout [post]
// @Security BearerAuth
func (h *Handler) CreateCheckout(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req CreateCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	url, err := h.service.CreateConversionCheckout(c.Context(), storeID, Plan(req.Plan))
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, fiber.Map{"url": url})
}

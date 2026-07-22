package billing

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

// Handler exposes the store-scoped billing endpoints. The AccessGuard
// allowlists everything under /billing so a blocked merchant can still pay.
type Handler struct {
	service *Service
}

// NewHandler builds the billing handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes mounts /billing under the store-scoped group.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/billing")
	g.Get("/subscription", h.GetSubscription)
	g.Post("/checkout", h.CreateCheckout)
	g.Post("/portal", h.CreatePortal)
	g.Post("/change-plan", h.ChangePlan)
	g.Get("/usage", h.GetUsage)
	g.Get("/statement", h.GetStatement)
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
	storeID := httpx.GetStoreID(c)

	sub, err := h.service.GetSubscription(c.UserContext(), storeID)
	if err != nil {
		return err
	}
	if sub == nil {
		// Legacy store — provision the trial lazily and return it.
		state, err := h.service.EnsureTrialSubscription(c.UserContext(), storeID, "", "")
		if err != nil {
			return err
		}
		return httpx.OK(c, state)
	}
	return httpx.OK(c, NewSubscriptionResponse(sub, config.PaywallEnabled.Bool(), time.Now()))
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
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/billing/checkout [post]
// @Security BearerAuth
func (h *Handler) CreateCheckout(c *fiber.Ctx) error {
	var req CreateCheckoutRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	url, err := h.service.CreateConversionCheckout(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, fiber.Map{"url": url})
}

// CreatePortal opens the Stripe Customer Portal (card, invoices, cancel).
// @Summary Open customer portal
// @Tags billing
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/billing/portal [post]
// @Security BearerAuth
func (h *Handler) CreatePortal(c *fiber.Ctx) error {
	url, err := h.service.CreatePortalSession(c.UserContext(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, fiber.Map{"url": url})
}

// ChangePlan upgrades immediately (prorated) or schedules a downgrade.
// @Summary Change plan
// @Tags billing
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body ChangePlanRequest true "Target plan"
// @Success 200 {object} httpx.Envelope{data=SubscriptionState}
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/billing/change-plan [post]
// @Security BearerAuth
func (h *Handler) ChangePlan(c *fiber.Ctx) error {
	var req ChangePlanRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	sub, err := h.service.ChangePlan(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewSubscriptionResponse(sub, config.PaywallEnabled.Bool(), time.Now()))
}

// GetUsage returns the current-cycle ledger summary (Financeiro hero).
// @Summary Get period usage
// @Tags billing
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope{data=PeriodUsage}
// @Router /api/v1/stores/{storeId}/billing/usage [get]
// @Security BearerAuth
func (h *Handler) GetUsage(c *fiber.Ctx) error {
	usage, err := h.service.GetUsage(c.UserContext(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, usage)
}

// GetStatement returns the paginated merchant extrato.
// @Summary Get billing statement
// @Tags billing
// @Produce json
// @Param storeId path string true "Store ID"
// @Param page query int false "Page" default(1)
// @Param limit query int false "Limit" default(30)
// @Success 200 {object} httpx.Envelope{data=[]StatementEntry}
// @Router /api/v1/stores/{storeId}/billing/statement [get]
// @Security BearerAuth
func (h *Handler) GetStatement(c *fiber.Ctx) error {
	entries, err := h.service.GetStatement(c.UserContext(), httpx.GetStoreID(c), c.QueryInt("page", 1), c.QueryInt("limit", 30))
	if err != nil {
		return err
	}
	return httpx.OK(c, entries)
}

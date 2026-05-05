package coupon

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
}

func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

// RegisterRoutes mounts the admin CRUD under each event. The router argument
// is expected to be the store-scoped group (the surrounding middleware sets
// store_id in c.Locals); here we just nest under /events/:eventId/coupons.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/events/:eventId/coupons")
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Patch("/:id", h.Update)
	g.Delete("/:id", h.Delete)
}

// RegisterPublicRoutes mounts the buyer-side apply / remove endpoints under
// /api/public so they bypass auth — same pattern the rest of the public
// checkout uses (token in path is the trust boundary).
func (h *Handler) RegisterPublicRoutes(app fiber.Router) {
	g := app.Group("/api/public/checkout/:token/coupon")
	g.Post("/", h.Apply)
	g.Delete("/", h.Remove)
}

// ApplyCouponRequest is the body for POST /api/public/checkout/:token/coupon.
type ApplyCouponRequest struct {
	Code string `json:"code" validate:"required,min=2,max=40"`
}

// Apply godoc
// @Summary      Apply a coupon to a public cart
// @Tags         coupons
// @Accept       json
// @Produce      json
// @Param        token path string true "Cart token"
// @Param        request body ApplyCouponRequest true "Coupon code"
// @Success      200 {object} httpx.Envelope{data=ApplyResult}
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/public/checkout/{token}/coupon [post]
func (h *Handler) Apply(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}

	var req ApplyCouponRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	out, err := h.service.ApplyToCart(c.Context(), token, req.Code)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, out)
}

// Remove godoc
// @Summary      Remove the applied coupon from a public cart
// @Tags         coupons
// @Produce      json
// @Param        token path string true "Cart token"
// @Success      204 "No content"
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Router       /api/public/checkout/{token}/coupon [delete]
func (h *Handler) Remove(c *fiber.Ctx) error {
	token := c.Params("token")
	if token == "" {
		return httpx.BadRequest(c, "token is required")
	}
	if err := h.service.RemoveFromCart(c.Context(), token); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// List godoc
// @Summary      List coupons for an event
// @Tags         coupons
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        eventId path string true "Event UUID"
// @Success      200 {object} httpx.Envelope{data=[]Coupon}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/events/{eventId}/coupons [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("eventId")

	coupons, err := h.service.List(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, coupons)
}

// GetByID godoc
// @Summary      Get a coupon by id
// @Tags         coupons
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=Coupon}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/events/{eventId}/coupons/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("eventId")
	id := c.Params("id")

	coupon, err := h.service.Get(c.Context(), id, eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, coupon)
}

// Create godoc
// @Summary      Create a coupon for an event
// @Tags         coupons
// @Accept       json
// @Produce      json
// @Param        request body CreateRequest true "Coupon to create"
// @Success      201 {object} httpx.Envelope{data=Coupon}
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/events/{eventId}/coupons [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("eventId")

	var req CreateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	coupon, err := h.service.Create(c.Context(), eventID, storeID, req)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Created(c, coupon)
}

// Update godoc
// @Summary      Update a coupon
// @Tags         coupons
// @Accept       json
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=Coupon}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/events/{eventId}/coupons/{id} [patch]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("eventId")
	id := c.Params("id")

	var req UpdateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	coupon, err := h.service.Update(c.Context(), id, eventID, storeID, req)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, coupon)
}

// Delete godoc
// @Summary      Delete a coupon (only when not yet redeemed)
// @Tags         coupons
// @Produce      json
// @Success      204 "No content"
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/events/{eventId}/coupons/{id} [delete]
// @Security     BearerAuth
func (h *Handler) Delete(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("eventId")
	id := c.Params("id")

	if err := h.service.Delete(c.Context(), id, eventID, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

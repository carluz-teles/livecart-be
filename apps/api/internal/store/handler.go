package store

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

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/stores")
	g.Post("/", h.Create)
	g.Get("/me", h.GetCurrent)
	g.Put("/me", h.Update)
}

// Create godoc
// @Summary      Create a new store
// @Description  Creates a new store with the given name and slug
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        request body CreateStoreRequest true "Store creation payload"
// @Success      201 {object} httpx.Envelope{data=CreateStoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	var req CreateStoreRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.Create(c.Context(), CreateStoreInput{
		Name: req.Name,
		Slug: req.Slug,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, CreateStoreResponse{
		ID:        output.ID,
		Name:      output.Name,
		Slug:      output.Slug,
		CreatedAt: output.CreatedAt,
	})
}

// GetCurrent godoc
// @Summary      Get current store
// @Description  Returns the store associated with the authenticated user
// @Tags         stores
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/me [get]
// @Security     BearerAuth
func (h *Handler) GetCurrent(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetByID(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, StoreResponse{
		ID:             output.ID,
		Name:           output.Name,
		Slug:           output.Slug,
		Active:         output.Active,
		WhatsappNumber: output.WhatsappNumber,
		EmailAddress:   output.EmailAddress,
		SMSNumber:      output.SMSNumber,
		CreatedAt:      output.CreatedAt,
	})
}

// Update godoc
// @Summary      Update current store
// @Description  Updates the store associated with the authenticated user
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        request body UpdateStoreRequest true "Store update payload"
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/me [put]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req UpdateStoreRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.Update(c.Context(), UpdateStoreInput{
		StoreID:        storeID,
		Name:           req.Name,
		WhatsappNumber: req.WhatsappNumber,
		EmailAddress:   req.EmailAddress,
		SMSNumber:      req.SMSNumber,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, StoreResponse{
		ID:             output.ID,
		Name:           output.Name,
		Slug:           output.Slug,
		Active:         output.Active,
		WhatsappNumber: output.WhatsappNumber,
		EmailAddress:   output.EmailAddress,
		SMSNumber:      output.SMSNumber,
		CreatedAt:      output.CreatedAt,
	})
}

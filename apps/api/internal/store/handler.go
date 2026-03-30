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
	g.Put("/me/cart-settings", h.UpdateCartSettings)
}

// RegisterStoreScopedRoutes registers routes under /stores/:storeId
func (h *Handler) RegisterStoreScopedRoutes(router fiber.Router) {
	router.Put("", h.UpdateByID)
	router.Put("/cart-settings", h.UpdateCartSettingsByID)
}

// Create godoc
// @Summary      Create a new store
// @Description  Creates a new store with owner membership
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        request body CreateStoreRequest true "Store creation payload"
// @Success      201 {object} httpx.Envelope{data=CreateStoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      401 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return httpx.Unauthorized(c, "unauthorized")
	}

	var req CreateStoreRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.Create(c.Context(), CreateStoreInput{
		Name:        req.Name,
		Slug:        req.Slug,
		ClerkUserID: clerkUserID,
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
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return httpx.Unauthorized(c, "unauthorized")
	}

	output, err := h.service.GetByClerkUserID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toStoreResponse(output))
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
	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return httpx.Forbidden(c, "no store associated with this user")
	}

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
		Description:    req.Description,
		Website:        req.Website,
		LogoURL:        req.LogoURL,
		Address:        req.Address,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toStoreResponse(output))
}

// UpdateCartSettings godoc
// @Summary      Update cart settings
// @Description  Updates the cart settings for the authenticated user's store
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        request body UpdateCartSettingsRequest true "Cart settings payload"
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/me/cart-settings [put]
// @Security     BearerAuth
func (h *Handler) UpdateCartSettings(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return httpx.Forbidden(c, "no store associated with this user")
	}

	var req UpdateCartSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.UpdateCartSettings(c.Context(), UpdateCartSettingsInput{
		StoreID:                storeID,
		Enabled:                req.Enabled,
		ExpirationMinutes:      req.ExpirationMinutes,
		ReserveStock:           req.ReserveStock,
		MaxItems:               req.MaxItems,
		MaxQuantityPerItem:     req.MaxQuantityPerItem,
		NotifyBeforeExpiration: req.NotifyBeforeExpiration,
		AllowEdit:              req.AllowEdit,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toStoreResponse(output))
}

// UpdateByID godoc
// @Summary      Update store by ID
// @Description  Updates a specific store (requires store access)
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body UpdateStoreRequest true "Store update payload"
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId} [put]
// @Security     BearerAuth
func (h *Handler) UpdateByID(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return httpx.Forbidden(c, "no store access")
	}

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
		Description:    req.Description,
		Website:        req.Website,
		LogoURL:        req.LogoURL,
		Address:        req.Address,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toStoreResponse(output))
}

// UpdateCartSettingsByID godoc
// @Summary      Update cart settings for a store
// @Description  Updates the cart settings for a specific store (requires store access)
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body UpdateCartSettingsRequest true "Cart settings payload"
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/cart-settings [put]
// @Security     BearerAuth
func (h *Handler) UpdateCartSettingsByID(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return httpx.Forbidden(c, "no store access")
	}

	var req UpdateCartSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.UpdateCartSettings(c.Context(), UpdateCartSettingsInput{
		StoreID:                storeID,
		Enabled:                req.Enabled,
		ExpirationMinutes:      req.ExpirationMinutes,
		ReserveStock:           req.ReserveStock,
		MaxItems:               req.MaxItems,
		MaxQuantityPerItem:     req.MaxQuantityPerItem,
		NotifyBeforeExpiration: req.NotifyBeforeExpiration,
		AllowEdit:              req.AllowEdit,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toStoreResponse(output))
}

func toStoreResponse(output StoreOutput) StoreResponse {
	return StoreResponse{
		ID:             output.ID,
		Name:           output.Name,
		Slug:           output.Slug,
		Active:         output.Active,
		WhatsappNumber: output.WhatsappNumber,
		EmailAddress:   output.EmailAddress,
		SMSNumber:      output.SMSNumber,
		Description:    output.Description,
		Website:        output.Website,
		LogoURL:        output.LogoURL,
		Address:        output.Address,
		CartSettings:   output.CartSettings,
		CreatedAt:      output.CreatedAt,
	}
}

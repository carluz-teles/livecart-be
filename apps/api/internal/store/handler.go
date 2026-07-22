package store

import (
	"github.com/gofiber/fiber/v2"

	storedomain "livecart/apps/api/internal/store/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/storage"
)

type Handler struct {
	service  *Service
	s3Client *storage.S3Client
}

func NewHandler(service *Service, s3Client *storage.S3Client) *Handler {
	return &Handler{service: service, s3Client: s3Client}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/stores")
	g.Post("/", h.Create)
	g.Get("/me", h.GetCurrent)
	g.Put("/me", h.Update)
	g.Put("/me/cart-settings", h.UpdateCartSettings)
	g.Put("/me/shipping-defaults", h.UpdateShippingDefaults)
	g.Post("/me/logo", h.UploadLogo)
}

// RegisterStoreScopedRoutes registers routes under /stores/:storeId
func (h *Handler) RegisterStoreScopedRoutes(router fiber.Router) {
	router.Put("", h.UpdateByID)
	router.Put("/cart-settings", h.UpdateCartSettingsByID)
	router.Put("/shipping-defaults", h.UpdateShippingDefaultsByID)
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
		return &httpx.ServiceError{Code: 401, Message: "unauthorized"}
	}

	var req CreateStoreRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	store, err := h.service.Create(c.UserContext(), req.ToInput(clerkUserID))
	if err != nil {
		return err
	}

	return httpx.Created(c, NewCreateStoreResponse(store))
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
		return &httpx.ServiceError{Code: 401, Message: "unauthorized"}
	}

	store, err := h.service.GetByClerkUserID(c.UserContext(), clerkUserID)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
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
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return &httpx.ServiceError{Code: 401, Message: "unauthorized"}
	}

	var req UpdateStoreRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	// /stores/me routes don't carry StoreAccessMiddleware, so resolve the store
	// for this user first.
	current, err := h.service.GetByClerkUserID(c.UserContext(), clerkUserID)
	if err != nil {
		return err
	}

	store, err := h.service.Update(c.UserContext(), req.ToInput(current.ID().String()))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
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
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return &httpx.ServiceError{Code: 401, Message: "unauthorized"}
	}

	var req UpdateCartSettingsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	current, err := h.service.GetByClerkUserID(c.UserContext(), clerkUserID)
	if err != nil {
		return err
	}

	store, err := h.service.UpdateCartSettings(c.UserContext(), req.ToInput(current.ID().String()))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
}

// UpdateShippingDefaults updates the store shipping defaults (package weight/format) for the authenticated user.
// @Summary      Update shipping defaults
// @Description  Updates the store shipping defaults for the authenticated user's store
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        request body UpdateShippingDefaultsRequest true "Shipping defaults payload"
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/me/shipping-defaults [put]
// @Security     BearerAuth
func (h *Handler) UpdateShippingDefaults(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return &httpx.ServiceError{Code: 401, Message: "unauthorized"}
	}

	var req UpdateShippingDefaultsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	current, err := h.service.GetByClerkUserID(c.UserContext(), clerkUserID)
	if err != nil {
		return err
	}

	store, err := h.service.UpdateShippingDefaults(c.UserContext(), req.ToInput(current.ID().String()))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
}

// UploadLogo godoc
// @Summary      Upload store logo
// @Description  Uploads a logo image for the authenticated user's store
// @Tags         stores
// @Accept       multipart/form-data
// @Produce      json
// @Param        file formData file true "Logo image file (JPG, PNG, GIF, max 2MB)"
// @Success      200 {object} httpx.Envelope{data=UploadLogoResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      413 {object} httpx.Envelope
// @Router       /api/v1/stores/me/logo [post]
// @Security     BearerAuth
func (h *Handler) UploadLogo(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return &httpx.ServiceError{Code: 401, Message: "unauthorized"}
	}

	current, err := h.service.GetByClerkUserID(c.UserContext(), clerkUserID)
	if err != nil {
		return err
	}
	storeID := current.ID().String()

	if h.s3Client == nil {
		return httpx.ErrInternal("storage not configured")
	}

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.ErrBadRequest("file is required")
	}

	// Validate file size (max 2MB)
	if file.Size > 2*1024*1024 {
		return httpx.ErrBadRequest("file too large, maximum size is 2MB")
	}

	// Validate content type
	contentType := file.Header.Get("Content-Type")
	validTypes := map[string]bool{
		"image/jpeg": true,
		"image/jpg":  true,
		"image/png":  true,
		"image/gif":  true,
		"image/webp": true,
	}
	if !validTypes[contentType] {
		return httpx.ErrBadRequest("invalid file type, accepted: JPG, PNG, GIF, WebP")
	}

	src, err := file.Open()
	if err != nil {
		return httpx.ErrInternal("failed to read file")
	}
	defer src.Close()

	// Upload to S3 (returns the S3 key, not a URL)
	folder := "logos/" + storeID
	key, err := h.s3Client.UploadFile(c.UserContext(), src, file.Filename, contentType, folder)
	if err != nil {
		return httpx.ErrInternal("failed to upload file")
	}

	store, err := h.service.UpdateLogoURL(c.UserContext(), storeID, key)
	if err != nil {
		return err
	}

	// Generate presigned URL for the response
	presignedURL, err := h.s3Client.GeneratePresignedGetURL(c.UserContext(), key, 0)
	if err != nil {
		return httpx.ErrInternal("failed to generate logo URL")
	}

	store.SetLogoURL(&presignedURL)
	return httpx.OK(c, UploadLogoResponse{
		LogoURL: presignedURL,
		Store:   NewStoreResponse(store),
	})
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
		return httpx.ErrForbidden("no store access")
	}

	var req UpdateStoreRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	store, err := h.service.Update(c.UserContext(), req.ToInput(storeID))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
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
		return httpx.ErrForbidden("no store access")
	}

	var req UpdateCartSettingsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	store, err := h.service.UpdateCartSettings(c.UserContext(), req.ToInput(storeID))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
}

// UpdateShippingDefaultsByID godoc
// @Summary      Update shipping defaults for a store
// @Description  Updates the store shipping defaults for a specific store (requires store access)
// @Tags         stores
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body UpdateShippingDefaultsRequest true "Shipping defaults payload"
// @Success      200 {object} httpx.Envelope{data=StoreResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/shipping-defaults [put]
// @Security     BearerAuth
func (h *Handler) UpdateShippingDefaultsByID(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return httpx.ErrForbidden("no store access")
	}

	var req UpdateShippingDefaultsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	store, err := h.service.UpdateShippingDefaults(c.UserContext(), req.ToInput(storeID))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewStoreResponse(h.presignLogo(c, store)))
}

// presignLogo swaps the stored S3 key on the entity for a presigned GET URL so
// the response carries a usable logo URL. It mutates the entity in-place and
// returns it for chaining. No-op when S3 is unconfigured or no logo is set.
func (h *Handler) presignLogo(c *fiber.Ctx, s *storedomain.Store) *storedomain.Store {
	if h.s3Client == nil {
		return s
	}
	key := s.LogoURL()
	if key == nil || *key == "" {
		return s
	}
	presignedURL, err := h.s3Client.GeneratePresignedGetURL(c.UserContext(), *key, 0)
	if err == nil && presignedURL != "" {
		s.SetLogoURL(&presignedURL)
	}
	return s
}

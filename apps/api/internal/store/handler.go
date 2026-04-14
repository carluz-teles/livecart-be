package store

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/storage"
)

type Handler struct {
	service   *Service
	validate  *validator.Validate
	s3Client  *storage.S3Client
}

func NewHandler(service *Service, validate *validator.Validate, s3Client *storage.S3Client) *Handler {
	return &Handler{service: service, validate: validate, s3Client: s3Client}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/stores")
	g.Post("/", h.Create)
	g.Get("/me", h.GetCurrent)
	g.Put("/me", h.Update)
	g.Put("/me/cart-settings", h.UpdateCartSettings)
	g.Post("/me/logo", h.UploadLogo)
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

	return httpx.OK(c, h.toStoreResponseWithPresignedLogo(c, output))
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
	// Get clerk user ID and look up store (since /stores/me routes don't have StoreAccessMiddleware)
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return httpx.Unauthorized(c, "unauthorized")
	}

	// Look up the store for this user
	storeOutput, err := h.service.GetByClerkUserID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	storeID := storeOutput.ID

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

	return httpx.OK(c, h.toStoreResponseWithPresignedLogo(c, output))
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
	// Get clerk user ID and look up store (since /stores/me routes don't have StoreAccessMiddleware)
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return httpx.Unauthorized(c, "unauthorized")
	}

	// Look up the store for this user
	storeOutput, err := h.service.GetByClerkUserID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	storeID := storeOutput.ID

	var req UpdateCartSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.UpdateCartSettings(c.Context(), UpdateCartSettingsInput{
		StoreID:                 storeID,
		Enabled:                 req.Enabled,
		ExpirationMinutes:       req.ExpirationMinutes,
		ReserveStock:            req.ReserveStock,
		MaxItems:                req.MaxItems,
		MaxQuantityPerItem:      req.MaxQuantityPerItem,
		NotifyBeforeExpiration:  req.NotifyBeforeExpiration,
		AllowEdit:               req.AllowEdit,
		AutoSendCheckoutLinks:   req.AutoSendCheckoutLinks,
		CheckoutLinkExpiryHours: req.CheckoutLinkExpiryHours,
		CheckoutSendMethods:     req.CheckoutSendMethods,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, h.toStoreResponseWithPresignedLogo(c, output))
}

// UploadLogo godoc
// @Summary      Upload store logo
// @Description  Uploads a logo image for the authenticated user's store
// @Tags         stores
// @Accept       multipart/form-data
// @Produce      json
// @Param        file formance file true "Logo image file (JPG, PNG, GIF, max 2MB)"
// @Success      200 {object} httpx.Envelope{data=UploadLogoResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      413 {object} httpx.Envelope
// @Router       /api/v1/stores/me/logo [post]
// @Security     BearerAuth
func (h *Handler) UploadLogo(c *fiber.Ctx) error {
	// Get clerk user ID and look up store (since /stores/me routes don't have StoreAccessMiddleware)
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return httpx.Unauthorized(c, "unauthorized")
	}

	// Look up the store for this user
	storeOutput, err := h.service.GetByClerkUserID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	storeID := storeOutput.ID

	if h.s3Client == nil {
		return httpx.InternalError(c, "storage not configured")
	}

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.BadRequest(c, "file is required")
	}

	// Validate file size (max 2MB)
	if file.Size > 2*1024*1024 {
		return httpx.BadRequest(c, "file too large, maximum size is 2MB")
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
		return httpx.BadRequest(c, "invalid file type, accepted: JPG, PNG, GIF, WebP")
	}

	// Open the file
	src, err := file.Open()
	if err != nil {
		return httpx.InternalError(c, "failed to read file")
	}
	defer src.Close()

	// Upload to S3
	folder := "logos/" + storeID
	// UploadFile now returns the S3 key (not a URL)
	key, err := h.s3Client.UploadFile(c.Context(), src, file.Filename, contentType, folder)
	if err != nil {
		return httpx.InternalError(c, "failed to upload file")
	}

	// Update store with new logo key
	output, err := h.service.UpdateLogoURL(c.Context(), storeID, key)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Generate presigned URL for the response
	presignedURL, err := h.s3Client.GeneratePresignedGetURL(c.Context(), key, 0)
	if err != nil {
		return httpx.InternalError(c, "failed to generate logo URL")
	}

	return httpx.OK(c, UploadLogoResponse{
		LogoURL: presignedURL,
		Store:   h.toStoreResponseWithPresignedLogo(c, output),
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

	return httpx.OK(c, h.toStoreResponseWithPresignedLogo(c, output))
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
		StoreID:                 storeID,
		Enabled:                 req.Enabled,
		ExpirationMinutes:       req.ExpirationMinutes,
		ReserveStock:            req.ReserveStock,
		MaxItems:                req.MaxItems,
		MaxQuantityPerItem:      req.MaxQuantityPerItem,
		NotifyBeforeExpiration:  req.NotifyBeforeExpiration,
		AllowEdit:               req.AllowEdit,
		AutoSendCheckoutLinks:   req.AutoSendCheckoutLinks,
		CheckoutLinkExpiryHours: req.CheckoutLinkExpiryHours,
		CheckoutSendMethods:     req.CheckoutSendMethods,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, h.toStoreResponseWithPresignedLogo(c, output))
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

// toStoreResponseWithPresignedLogo converts output to response and generates presigned URL for logo
func (h *Handler) toStoreResponseWithPresignedLogo(c *fiber.Ctx, output StoreOutput) StoreResponse {
	resp := toStoreResponse(output)

	// Generate presigned URL for logo if S3 client is available and logo exists
	if h.s3Client != nil && output.LogoURL != nil && *output.LogoURL != "" {
		presignedURL, err := h.s3Client.GeneratePresignedGetURL(c.Context(), *output.LogoURL, 0)
		if err == nil && presignedURL != "" {
			resp.LogoURL = &presignedURL
		}
	}

	return resp
}

package user

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
	g := router.Group("/users")
	g.Get("/me", h.GetMe)
	g.Get("/me/stores", h.GetMyStores)
	g.Post("/sync", h.SyncUser)
}

// GetMe godoc
// @Summary      Get current user
// @Description  Returns the authenticated user's profile including store information
// @Tags         users
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=GetMeResponse}
// @Failure      401 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/users/me [get]
// @Security     BearerAuth
func (h *Handler) GetMe(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
	}

	output, err := h.service.GetByClerkID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, GetMeResponse{
		ID:        output.ID,
		StoreID:   output.StoreID,
		Email:     output.Email,
		Name:      output.Name,
		AvatarURL: output.AvatarURL,
		Role:      output.Role,
		Status:    output.Status,
		StoreName: output.StoreName,
		StoreSlug: output.StoreSlug,
		CreatedAt: output.CreatedAt,
		UpdatedAt: output.UpdatedAt,
	})
}

// GetMyStores godoc
// @Summary      Get all stores for current user
// @Description  Returns all stores the authenticated user belongs to
// @Tags         users
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=GetUserStoresResponse}
// @Failure      401 {object} httpx.Envelope
// @Router       /api/v1/users/me/stores [get]
// @Security     BearerAuth
func (h *Handler) GetMyStores(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
	}

	stores, err := h.service.GetUserStores(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]UserStoreResponse, len(stores))
	for i, s := range stores {
		responses[i] = UserStoreResponse{
			ID:        s.ID,
			StoreID:   s.StoreID,
			Role:      s.Role,
			Status:    s.Status,
			StoreName: s.StoreName,
			StoreSlug: s.StoreSlug,
			CreatedAt: s.CreatedAt,
		}
	}

	return httpx.OK(c, GetUserStoresResponse{Stores: responses})
}

// SyncUser godoc
// @Summary      Sync user on first access
// @Description  Creates a new user and store if not exists, or returns existing user. Call this after first sign-in.
// @Tags         users
// @Accept       json
// @Produce      json
// @Param        request body SyncUserRequest true "Store information for new users"
// @Success      200 {object} httpx.Envelope{data=GetMeResponse}
// @Success      201 {object} httpx.Envelope{data=GetMeResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      401 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/users/sync [post]
// @Security     BearerAuth
func (h *Handler) SyncUser(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
	}

	var req SyncUserRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	claims := httpx.GetClaims(c)
	email := ""
	name := ""
	avatarURL := ""
	if claims != nil {
		email = claims.Email
		name = claims.FullName()
		avatarURL = claims.ImageURL
	}

	output, err := h.service.SyncUser(c.Context(), SyncUserInput{
		ClerkUserID: clerkUserID,
		Email:       email,
		Name:        name,
		AvatarURL:   avatarURL,
		StoreName:   req.StoreName,
		StoreSlug:   req.StoreSlug,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	response := GetMeResponse{
		ID:        output.ID,
		StoreID:   output.StoreID,
		Email:     output.Email,
		Name:      output.Name,
		AvatarURL: output.AvatarURL,
		Role:      output.Role,
		Status:    output.Status,
		StoreName: output.StoreName,
		StoreSlug: output.StoreSlug,
		CreatedAt: output.CreatedAt,
		UpdatedAt: output.UpdatedAt,
	}

	if output.IsNew {
		return httpx.Created(c, response)
	}
	return httpx.OK(c, response)
}

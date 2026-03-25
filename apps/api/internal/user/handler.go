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
	g.Post("/sync", h.SyncUser)
	g.Get("/me/stores", h.GetMyStores)
	g.Post("/me/select-store", h.SelectStore)
}

// SyncUser godoc
// @Summary      Sync user on login
// @Description  Creates/updates user and returns all memberships for the authenticated user. Does NOT create stores automatically.
// @Tags         users
// @Accept       json
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=SyncUserResponse}
// @Failure      401 {object} httpx.Envelope
// @Router       /api/v1/users/sync [post]
// @Security     BearerAuth
func (h *Handler) SyncUser(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
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
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Convert service output to response
	memberships := make([]MembershipResponse, len(output.Memberships))
	for i, m := range output.Memberships {
		memberships[i] = MembershipResponse{
			ID:             m.ID,
			StoreID:        m.StoreID,
			StoreName:      m.StoreName,
			StoreSlug:      m.StoreSlug,
			Role:           m.Role,
			Status:         m.Status,
			Email:          m.Email,
			Name:           m.Name,
			AvatarURL:      m.AvatarURL,
			LastAccessedAt: m.LastAccessedAt,
			CreatedAt:      m.CreatedAt,
		}
	}

	return httpx.OK(c, SyncUserResponse{
		UserID:              output.UserID,
		ClerkUserID:         output.ClerkUserID,
		Email:               output.Email,
		Name:                output.Name,
		AvatarURL:           output.AvatarURL,
		Memberships:         memberships,
		LastAccessedStoreID: output.LastAccessedStoreID,
		State:               output.State,
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

	// Get user ID from clerk ID
	userID, err := h.service.GetUserIDByClerkID(c.Context(), clerkUserID)
	if err != nil {
		// If user not found, return empty list
		if httpx.IsNotFound(err) {
			return httpx.OK(c, GetUserStoresResponse{Stores: []MembershipResponse{}})
		}
		return httpx.HandleServiceError(c, err)
	}

	stores, err := h.service.GetUserStores(c.Context(), userID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]MembershipResponse, len(stores))
	for i, m := range stores {
		responses[i] = MembershipResponse{
			ID:             m.ID,
			StoreID:        m.StoreID,
			StoreName:      m.StoreName,
			StoreSlug:      m.StoreSlug,
			Role:           m.Role,
			Status:         m.Status,
			Email:          m.Email,
			Name:           m.Name,
			AvatarURL:      m.AvatarURL,
			LastAccessedAt: m.LastAccessedAt,
			CreatedAt:      m.CreatedAt,
		}
	}

	return httpx.OK(c, GetUserStoresResponse{Stores: responses})
}

// SelectStore godoc
// @Summary      Select active store
// @Description  Updates the last accessed store for the user
// @Tags         users
// @Accept       json
// @Produce      json
// @Param        request body SelectStoreRequest true "Store to select"
// @Success      200 {object} httpx.Envelope
// @Failure      400 {object} httpx.Envelope
// @Failure      401 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/users/me/select-store [post]
// @Security     BearerAuth
func (h *Handler) SelectStore(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
	}

	var req SelectStoreRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Get user ID from clerk ID
	userID, err := h.service.GetUserIDByClerkID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	err = h.service.SelectStore(c.Context(), userID, req.StoreID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, nil)
}

// GetMe godoc
// @Summary      Get current user in store context
// @Description  Returns the authenticated user's membership info for the current store
// @Tags         users
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=GetMeResponse}
// @Failure      401 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/me [get]
// @Security     BearerAuth
func (h *Handler) GetMe(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
	}

	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return c.Status(fiber.StatusForbidden).JSON(httpx.Envelope{Error: "no store context"})
	}

	// Get user ID from clerk ID
	userID, err := h.service.GetUserIDByClerkID(c.Context(), clerkUserID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	m, err := h.service.GetMembership(c.Context(), userID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, GetMeResponse{
		ID:        m.ID,
		UserID:    m.UserID,
		StoreID:   m.StoreID,
		Email:     m.Email,
		Name:      m.Name,
		AvatarURL: m.AvatarURL,
		Role:      m.Role,
		Status:    m.Status,
		StoreName: m.StoreName,
		StoreSlug: m.StoreSlug,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	})
}

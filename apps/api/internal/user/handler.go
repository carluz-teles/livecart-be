package user

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/storage"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
	s3Client *storage.S3Client
}

func NewHandler(service *Service, validate *validator.Validate, s3Client *storage.S3Client) *Handler {
	return &Handler{service: service, validate: validate, s3Client: s3Client}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/users")
	g.Post("/sync", h.SyncUser)
}

// SyncUser godoc
// @Summary      Sync user on login
// @Description  Creates/updates user and returns the single membership for the authenticated user (1 user = 1 store)
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
	var membership *MembershipResponse
	if output.Membership != nil {
		membership = &MembershipResponse{
			ID:           output.Membership.ID,
			StoreID:      output.Membership.StoreID,
			StoreName:    output.Membership.StoreName,
			StoreSlug:    output.Membership.StoreSlug,
			StoreLogoURL: output.Membership.StoreLogoURL,
			Role:         output.Membership.Role,
			Status:       output.Membership.Status,
			Email:        output.Membership.Email,
			Name:         output.Membership.Name,
			AvatarURL:    output.Membership.AvatarURL,
			CreatedAt:    output.Membership.CreatedAt,
		}

		// Generate presigned URL for store logo if available
		if h.s3Client != nil && membership.StoreLogoURL != nil && *membership.StoreLogoURL != "" {
			presignedURL, err := h.s3Client.GeneratePresignedGetURL(c.Context(), *membership.StoreLogoURL, 0)
			if err == nil && presignedURL != "" {
				membership.StoreLogoURL = &presignedURL
			}
		}
	}

	return httpx.OK(c, SyncUserResponse{
		UserID:      output.UserID,
		ClerkUserID: output.ClerkUserID,
		Email:       output.Email,
		Name:        output.Name,
		AvatarURL:   output.AvatarURL,
		Membership:  membership,
		State:       output.State,
	})
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

	m, err := h.service.GetMembership(c.Context(), userID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if m == nil {
		return c.Status(fiber.StatusNotFound).JSON(httpx.Envelope{Error: "no membership found"})
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

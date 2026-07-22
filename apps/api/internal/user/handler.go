package user

import (
	"github.com/gofiber/fiber/v2"

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
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/users/sync [post]
// @Security     BearerAuth
func (h *Handler) SyncUser(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return &httpx.ServiceError{Code: fiber.StatusUnauthorized, Message: "unauthorized"}
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

	// Sync is bodyless: it derives its data from the JWT claims, so we build the
	// input via ToInput directly (no body to parse/validate).
	input, err := SyncUserRequest{}.ToInput(clerkUserID, email, name, avatarURL)
	if err != nil {
		return err
	}

	out, err := h.service.SyncUser(c.UserContext(), input)
	if err != nil {
		return err
	}

	// Presigned URL for the store logo is a presentation concern applied to the
	// outbound response (needs the per-request S3 client).
	if h.s3Client != nil && out.Membership != nil &&
		out.Membership.StoreLogoURL != nil && *out.Membership.StoreLogoURL != "" {
		if presignedURL, perr := h.s3Client.GeneratePresignedGetURL(c.UserContext(), *out.Membership.StoreLogoURL, 0); perr == nil && presignedURL != "" {
			out.Membership.StoreLogoURL = &presignedURL
		}
	}

	return httpx.OK(c, NewSyncUserResponse(out))
}

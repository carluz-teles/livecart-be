package invitation

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
}

func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

// RegisterRoutes registers invitation routes under /stores/:storeId/invitations
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/invitations")
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Post("/:id/resend", h.Resend)
	g.Delete("/:id", h.Revoke)
}

// RegisterPublicRoutes registers public invitation routes (no auth required)
func (h *Handler) RegisterPublicRoutes(router fiber.Router) {
	g := router.Group("/invitations")
	g.Get("/token/:token", h.GetByToken)
}

// RegisterAcceptRoute registers the accept invitation route (requires auth)
func (h *Handler) RegisterAcceptRoute(router fiber.Router) {
	g := router.Group("/invitations")
	g.Post("/accept", h.Accept)
}

// Create godoc
// @Summary      Create invitation
// @Description  Creates an invitation for a user to join the store
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body CreateInvitationRequest true "Invitation details"
// @Success      201 {object} httpx.Envelope{data=InvitationResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/invitations [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)
	memberIDStr := httpx.GetStoreUserID(c)

	var req CreateInvitationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Convert to value objects
	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	email, err := vo.NewEmail(req.Email)
	if err != nil {
		return httpx.BadRequest(c, "invalid email format")
	}

	role, err := vo.NewRole(req.Role)
	if err != nil {
		return httpx.BadRequest(c, "invalid role")
	}

	inviterID, err := vo.NewMemberID(memberIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid inviter ID")
	}

	output, err := h.service.Create(c.Context(), CreateInvitationInput{
		StoreID:   storeID,
		InviterID: inviterID,
		Email:     email,
		Role:      role,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, InvitationResponse{
		ID:        output.ID,
		Email:     output.Email,
		Role:      output.Role,
		Status:    output.Status,
		ExpiresAt: output.ExpiresAt,
		CreatedAt: output.CreatedAt,
	})
}

// List godoc
// @Summary      List invitations
// @Description  Returns all invitations for the store
// @Tags         invitations
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=ListInvitationsResponse}
// @Router       /api/v1/stores/{storeId}/invitations [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	invitations, err := h.service.List(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]InvitationResponse, len(invitations))
	for i, inv := range invitations {
		responses[i] = InvitationResponse{
			ID:          inv.ID,
			Email:       inv.Email,
			Role:        inv.Role,
			Status:      inv.Status,
			InviterName: inv.InviterName,
			ExpiresAt:   inv.ExpiresAt,
			AcceptedAt:  inv.AcceptedAt,
			CreatedAt:   inv.CreatedAt,
		}
	}

	return httpx.OK(c, ListInvitationsResponse{Data: responses})
}

// Revoke godoc
// @Summary      Revoke invitation
// @Description  Revokes a pending invitation
// @Tags         invitations
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Invitation UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/invitations/{id} [delete]
// @Security     BearerAuth
func (h *Handler) Revoke(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)
	id := c.Params("id")

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	invitationID, err := vo.NewInvitationID(id)
	if err != nil {
		return httpx.BadRequest(c, "invalid invitation ID")
	}

	if err := h.service.Revoke(c.Context(), storeID, invitationID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, id)
}

// Resend godoc
// @Summary      Resend invitation
// @Description  Generates a new token and resends the invitation email
// @Tags         invitations
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Invitation UUID"
// @Success      200 {object} httpx.Envelope{data=InvitationResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/invitations/{id}/resend [post]
// @Security     BearerAuth
func (h *Handler) Resend(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)
	memberIDStr := httpx.GetStoreUserID(c)
	id := c.Params("id")

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	invitationID, err := vo.NewInvitationID(id)
	if err != nil {
		return httpx.BadRequest(c, "invalid invitation ID")
	}

	inviterID, err := vo.NewMemberID(memberIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid inviter ID")
	}

	output, err := h.service.Resend(c.Context(), ResendInvitationInput{
		StoreID:      storeID,
		InvitationID: invitationID,
		InviterID:    inviterID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, InvitationResponse{
		ID:        output.ID,
		Email:     output.Email,
		Role:      output.Role,
		Status:    output.Status,
		ExpiresAt: output.ExpiresAt,
		CreatedAt: output.CreatedAt,
	})
}

// GetByToken godoc
// @Summary      Get invitation by token
// @Description  Returns invitation details for the accept page (public endpoint)
// @Tags         invitations
// @Produce      json
// @Param        token path string true "Invitation token"
// @Success      200 {object} httpx.Envelope{data=InvitationDetailsResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      410 {object} httpx.Envelope
// @Router       /api/v1/invitations/token/{token} [get]
func (h *Handler) GetByToken(c *fiber.Ctx) error {
	token := c.Params("token")

	output, err := h.service.GetByToken(c.Context(), token)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, InvitationDetailsResponse{
		ID:          output.ID,
		Email:       output.Email,
		Role:        output.Role,
		Status:      output.Status,
		StoreName:   output.StoreName,
		StoreSlug:   output.StoreSlug,
		InviterName: output.InviterName,
		ExpiresAt:   output.ExpiresAt,
		CreatedAt:   output.CreatedAt,
	})
}

// Accept godoc
// @Summary      Accept invitation
// @Description  Accepts an invitation and adds the user to the store
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Param        request body AcceptInvitationRequest true "Invitation token"
// @Success      200 {object} httpx.Envelope{data=AcceptInvitationOutput}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      410 {object} httpx.Envelope
// @Router       /api/v1/invitations/accept [post]
// @Security     BearerAuth
func (h *Handler) Accept(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "unauthorized"})
	}

	var req AcceptInvitationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Get email from claims
	claims := httpx.GetClaims(c)
	emailStr := ""
	if claims != nil {
		emailStr = claims.Email
	}

	if emailStr == "" {
		return httpx.BadRequest(c, "email not found in token claims")
	}

	email, err := vo.NewEmail(emailStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid email: "+emailStr)
	}

	output, err := h.service.Accept(c.Context(), AcceptInvitationInput{
		Token:       req.Token,
		ClerkUserID: clerkUserID,
		Email:       email,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, output)
}

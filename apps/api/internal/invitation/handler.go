package invitation

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
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
	var req CreateInvitationRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), httpx.GetStoreUserID(c))
	if err != nil {
		return err
	}
	inv, err := h.service.Create(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.Created(c, NewInvitationResponse(inv))
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
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	invitations, err := h.service.List(c.UserContext(), storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewListInvitationsResponse(invitations))
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
	id := c.Params("id")
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	invitationID, err := vo.NewInvitationID(id)
	if err != nil {
		return httpx.ErrUnprocessable("invalid invitation ID")
	}
	if err := h.service.Revoke(c.UserContext(), storeID, invitationID); err != nil {
		return err
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
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	invitationID, err := vo.NewInvitationID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid invitation ID")
	}
	inviterID, err := vo.NewMemberID(httpx.GetStoreUserID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid inviter ID")
	}
	inv, err := h.service.Resend(c.UserContext(), ResendInvitationInput{
		StoreID:      storeID,
		InvitationID: invitationID,
		InviterID:    inviterID,
	})
	if err != nil {
		return err
	}
	return httpx.OK(c, NewInvitationResponse(inv))
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
	inv, err := h.service.GetByToken(c.UserContext(), c.Params("token"))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewInvitationDetailsResponse(inv))
}

// Accept godoc
// @Summary      Accept invitation
// @Description  Accepts an invitation and adds the user to the store
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Param        request body AcceptInvitationRequest true "Invitation token"
// @Success      200 {object} httpx.Envelope{data=AcceptInvitationResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      403 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      410 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/invitations/accept [post]
// @Security     BearerAuth
func (h *Handler) Accept(c *fiber.Ctx) error {
	clerkUserID := httpx.GetUserID(c)
	if clerkUserID == "" {
		return &httpx.ServiceError{Code: fiber.StatusUnauthorized, Message: "unauthorized"}
	}

	var req AcceptInvitationRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	// Email comes from the authenticated JWT claims, not the request body.
	claims := httpx.GetClaims(c)
	emailStr := ""
	if claims != nil {
		emailStr = claims.Email
	}
	if emailStr == "" {
		return httpx.ErrBadRequest("email not found in token claims")
	}

	input, err := req.ToInput(clerkUserID, emailStr)
	if err != nil {
		return err
	}
	inv, err := h.service.Accept(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewAcceptInvitationResponse(inv))
}

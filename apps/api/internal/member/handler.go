package member

import (
	"errors"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

type Handler struct {
	svc      *Service
	validate *validator.Validate
}

func NewHandler(svc *Service, validate *validator.Validate) *Handler {
	return &Handler{svc: svc, validate: validate}
}

func (h *Handler) RegisterRoutes(r fiber.Router) {
	members := r.Group("/members")
	members.Get("/", h.List)
	members.Patch("/:memberId/role", httpx.RequireRole(RoleOwner, RoleAdmin), h.UpdateRole)
	members.Delete("/:memberId", httpx.RequireRole(RoleOwner, RoleAdmin), h.Remove)
}

// List godoc
// @Summary      List store members
// @Description  Returns all members of the store
// @Tags         members
// @Accept       json
// @Produce      json
// @Param        storeId  path      string  true  "Store ID"
// @Success      200      {object}  httpx.Envelope{data=ListMembersResponse}
// @Failure      401      {object}  httpx.Envelope
// @Failure      403      {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/stores/{storeId}/members [get]
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)

	members, err := h.svc.List(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	resp := make([]MemberResponse, len(members))
	for i, m := range members {
		resp[i] = MemberResponse{
			ID:        m.ID,
			Email:     m.Email,
			Name:      m.Name,
			AvatarURL: m.AvatarURL,
			Role:      m.Role,
			Status:    m.Status,
			JoinedAt:  m.JoinedAt,
			InvitedAt: m.InvitedAt,
		}
	}

	return httpx.OK(c, ListMembersResponse{Data: resp})
}

// UpdateRole godoc
// @Summary      Update member role
// @Description  Updates the role of a store member (owner/admin only)
// @Tags         members
// @Accept       json
// @Produce      json
// @Param        storeId   path      string                   true  "Store ID"
// @Param        memberId  path      string                   true  "Member ID"
// @Param        body      body      UpdateMemberRoleRequest  true  "Role update request"
// @Success      200       {object}  httpx.Envelope{data=MemberResponse}
// @Failure      400       {object}  httpx.Envelope
// @Failure      401       {object}  httpx.Envelope
// @Failure      403       {object}  httpx.Envelope
// @Failure      404       {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/stores/{storeId}/members/{memberId}/role [patch]
func (h *Handler) UpdateRole(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	memberID := c.Params("memberId")

	var req UpdateMemberRoleRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	member, err := h.svc.UpdateRole(c.Context(), UpdateMemberRoleInput{
		StoreID:  storeID,
		MemberID: memberID,
		Role:     req.Role,
	})
	if err != nil {
		var invalidRoleErr *InvalidRoleError
		if errors.As(err, &invalidRoleErr) {
			return httpx.HandleServiceError(c, httpx.ErrUnprocessable(err.Error()))
		}
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, MemberResponse{
		ID:        member.ID,
		Email:     member.Email,
		Name:      member.Name,
		AvatarURL: member.AvatarURL,
		Role:      member.Role,
		Status:    member.Status,
		JoinedAt:  member.JoinedAt,
		InvitedAt: member.InvitedAt,
	})
}

// Remove godoc
// @Summary      Remove member from store
// @Description  Removes a member from the store (owner/admin only)
// @Tags         members
// @Accept       json
// @Produce      json
// @Param        storeId   path  string  true  "Store ID"
// @Param        memberId  path  string  true  "Member ID"
// @Success      204
// @Failure      400       {object}  httpx.Envelope
// @Failure      401       {object}  httpx.Envelope
// @Failure      403       {object}  httpx.Envelope
// @Failure      404       {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/stores/{storeId}/members/{memberId} [delete]
func (h *Handler) Remove(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	memberID := c.Params("memberId")
	requestingUserID := httpx.GetStoreUserID(c)

	err := h.svc.Remove(c.Context(), storeID, memberID, requestingUserID)
	if err != nil {
		var selfRemovalErr *SelfRemovalError
		var ownerErr *CannotRemoveOwnerError
		if errors.As(err, &selfRemovalErr) {
			return httpx.HandleServiceError(c, httpx.ErrForbidden(err.Error()))
		}
		if errors.As(err, &ownerErr) {
			return httpx.HandleServiceError(c, httpx.ErrForbidden(err.Error()))
		}
		return httpx.HandleServiceError(c, err)
	}

	return httpx.NoContent(c)
}

package member

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(r fiber.Router) {
	members := r.Group("/members")
	members.Get("/", h.List)
	members.Patch("/:memberId/role", httpx.RequireRole("owner", "admin"), h.UpdateRole)
	members.Delete("/:memberId", httpx.RequireRole("owner", "admin"), h.Remove)
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
	members, err := h.svc.List(c.Context(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewListMembersResponse(members))
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
// @Failure      422       {object}  httpx.ValidationEnvelope
// @Security     BearerAuth
// @Router       /api/v1/stores/{storeId}/members/{memberId}/role [patch]
func (h *Handler) UpdateRole(c *fiber.Ctx) error {
	var req UpdateMemberRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), c.Params("memberId"), httpx.GetStoreUserID(c))
	if err != nil {
		return err
	}
	member, err := h.svc.UpdateRole(c.Context(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewMemberResponse(member))
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
// @Failure      401       {object}  httpx.Envelope
// @Failure      403       {object}  httpx.Envelope
// @Failure      404       {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/stores/{storeId}/members/{memberId} [delete]
func (h *Handler) Remove(c *fiber.Ctx) error {
	err := h.svc.Remove(c.Context(), RemoveMemberInput{
		StoreID:  httpx.GetStoreID(c),
		MemberID: c.Params("memberId"),
		ActorID:  httpx.GetStoreUserID(c),
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(c)
}

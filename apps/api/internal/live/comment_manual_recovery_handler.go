package live

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// RecoverComment explicitly authorizes recovery of one unfulfilled comment.
// @Summary Recover a no_product comment (owner/admin)
// @Tags lives
// @Accept json
// @Produce json
// @Param storeId path string true "Store UUID"
// @Param id path string true "Event UUID"
// @Param commentId path string true "Internal comment UUID"
// @Param request body RecoverCommentRequest true "Explicit merchant confirmation"
// @Success 200 {object} httpx.Envelope{data=RecoverCommentResponse}
// @Failure 403 {object} httpx.Envelope
// @Failure 404 {object} httpx.Envelope
// @Failure 409 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Security BearerAuth
// @Router /api/v1/stores/{storeId}/lives/{id}/comments/{commentId}/recover [post]
func (h *Handler) RecoverComment(c *fiber.Ctx) error {
	var req RecoverCommentRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), c.Params("id"), c.Params("commentId"), httpx.GetStoreUserID(c))
	if err != nil {
		return err
	}
	recovery, err := h.service.RecoverComment(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewRecoverCommentResponse(recovery))
}

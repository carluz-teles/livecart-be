package live

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// LinkSessionMedia godoc
// @Summary      Link an Instagram media to a specific session
// @Description  Attaches the publication (or live) this session should capture. Unlike POST /platforms, which resolves the session by itself, this one is anchored on the sessionId — the only form that works in a campaign with more than one broadcast.
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Param        request body LinkSessionMediaRequest true "Media to link"
// @Success      201 {object} httpx.Envelope{data=PlatformResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/platforms [post]
// @Security     BearerAuth
func (h *Handler) LinkSessionMedia(c *fiber.Ctx) error {
	var req LinkSessionMediaRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	output, err := h.service.LinkSessionMedia(c.UserContext(), req.ToInput(
		c.Locals("store_id").(string), c.Params("id"), c.Params("sessionId"),
	))
	if err != nil {
		return err
	}

	return httpx.Created(c, PlatformResponse{
		ID:             output.ID,
		Platform:       output.Platform,
		PlatformLiveID: output.PlatformLiveID,
		AddedAt:        output.AddedAt,
	})
}

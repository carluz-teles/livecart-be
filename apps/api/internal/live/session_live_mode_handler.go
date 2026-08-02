package live

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// MODO LIVE POR SESSÃO (D17)
//
// Estado EFÊMERO de execução: produto em destaque e pausa do processamento.
// No evento, duas transmissões simultâneas compartilhavam o mesmo destaque e o
// estado residual de uma contaminava a outra.
// =============================================================================

// GetSessionLiveModeState godoc
// @Summary      Live mode state of a session
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Success      200 {object} httpx.Envelope{data=LiveModeStateResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/live-mode [get]
// @Security     BearerAuth
func (h *Handler) GetSessionLiveModeState(c *fiber.Ctx) error {
	state, err := h.service.GetSessionLiveModeState(
		c.UserContext(),
		c.Params("sessionId"),
		c.Params("id"),
		c.Locals("store_id").(string),
	)
	if err != nil {
		return err
	}
	return httpx.OK(c, toLiveModeStateResponse(state))
}

// SetSessionActiveProduct godoc
// @Summary      Set or clear the highlighted product of a session
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Param        request body SetSessionActiveProductRequest true "Active product"
// @Success      200 {object} httpx.Envelope{data=LiveModeStateResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/active-product [patch]
// @Security     BearerAuth
func (h *Handler) SetSessionActiveProduct(c *fiber.Ctx) error {
	var req SetSessionActiveProductRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	state, err := h.service.SetSessionActiveProduct(
		c.UserContext(),
		c.Params("sessionId"),
		c.Params("id"),
		c.Locals("store_id").(string),
		req.ProductID,
	)
	if err != nil {
		return err
	}
	return httpx.OK(c, toLiveModeStateResponse(state))
}

// SetSessionProcessingPaused godoc
// @Summary      Pause or resume comment processing for a session
// @Description  Pausar uma sessão não pausa as outras do mesmo evento.
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Param        request body SetSessionProcessingPausedRequest true "Processing state"
// @Success      200 {object} httpx.Envelope{data=LiveModeStateResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/pause-processing [patch]
// @Security     BearerAuth
func (h *Handler) SetSessionProcessingPaused(c *fiber.Ctx) error {
	var req SetSessionProcessingPausedRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	state, err := h.service.SetSessionProcessingPaused(
		c.UserContext(),
		c.Params("sessionId"),
		c.Params("id"),
		c.Locals("store_id").(string),
		req.Paused,
	)
	if err != nil {
		return err
	}
	return httpx.OK(c, toLiveModeStateResponse(state))
}

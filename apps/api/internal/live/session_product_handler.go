package live

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// SESSION PRODUCTS (Whitelist da SESSÃO — D15/N2)
//
// Regra única: lista vazia = TODOS os produtos da loja liberados naquela
// sessão. Sessão nova nasce vazia, portanto vende tudo, mesmo que outra sessão
// do mesmo evento tenha whitelist.
// =============================================================================

// ListSessionProducts godoc
// @Summary      List the session whitelist
// @Description  Products allowed in this session. Empty list means every store product is allowed.
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Success      200 {object} httpx.Envelope{data=ListSessionProductsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/whitelist [get]
// @Security     BearerAuth
func (h *Handler) ListSessionProducts(c *fiber.Ctx) error {
	products, err := h.service.ListSessionProducts(
		c.UserContext(),
		c.Params("sessionId"),
		c.Params("id"),
		c.Locals("store_id").(string),
	)
	if err != nil {
		return err
	}

	responses := make([]EventProductResponse, len(products))
	for i, p := range products {
		responses[i] = toEventProductResponse(p)
	}
	return httpx.OK(c, ListSessionProductsResponse{Data: responses})
}

// AddSessionProduct godoc
// @Summary      Add a product to the session whitelist
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Param        request body SessionProductRequest true "Product configuration"
// @Success      201 {object} httpx.Envelope{data=EventProductResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/whitelist [post]
// @Security     BearerAuth
func (h *Handler) AddSessionProduct(c *fiber.Ctx) error {
	var req SessionProductRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	output, err := h.service.UpsertSessionProduct(c.UserContext(), req.ToInput(
		c.Locals("store_id").(string), c.Params("id"), c.Params("sessionId"), "",
	))
	if err != nil {
		return err
	}
	return httpx.Created(c, toEventProductResponse(output))
}

// UpdateSessionProduct godoc
// @Summary      Update a product in the session whitelist
// @Description  Keyed by productId — never by the whitelist row id.
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Param        productId path string true "Product UUID"
// @Param        request body UpdateSessionProductRequest true "Product configuration"
// @Success      200 {object} httpx.Envelope{data=EventProductResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/whitelist/{productId} [put]
// @Security     BearerAuth
func (h *Handler) UpdateSessionProduct(c *fiber.Ctx) error {
	var req UpdateSessionProductRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	output, err := h.service.UpsertSessionProduct(c.UserContext(), req.ToInput(
		c.Locals("store_id").(string), c.Params("id"), c.Params("sessionId"), c.Params("productId"),
	))
	if err != nil {
		return err
	}
	return httpx.OK(c, toEventProductResponse(output))
}

// DeleteSessionProduct godoc
// @Summary      Remove a product from the session whitelist
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        sessionId path string true "Session UUID"
// @Param        productId path string true "Product UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/whitelist/{productId} [delete]
// @Security     BearerAuth
func (h *Handler) DeleteSessionProduct(c *fiber.Ctx) error {
	productID := c.Params("productId")
	err := h.service.DeleteSessionProduct(
		c.UserContext(),
		c.Params("sessionId"),
		c.Params("id"),
		c.Locals("store_id").(string),
		productID,
	)
	if err != nil {
		return err
	}
	return httpx.Deleted(c, productID)
}

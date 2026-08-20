package order

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/orders")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
	g.Get("/product-breakdown", h.ProductBreakdown)
	g.Get("/:id", h.GetByID)
	g.Get("/:id/upsell", h.GetUpsell)
	g.Patch("/:id", h.Update)
	g.Patch("/:id/shipping-address", h.UpdateShippingAddress)
	g.Post("/:id/regenerate-checkout", h.RegenerateCheckout)
	g.Post("/:id/cancel", h.Cancel)
	g.Post("/:id/retry-erp", h.RetryERPFinalisation)
	g.Post("/:id/sync-invoice", h.SyncInvoice)
	// Edição dos itens pelo lojista, para pedido ainda não pago. Ver item_edit.go.
	g.Post("/:id/items", h.AddItem)
	g.Patch("/:id/items/:itemId", h.SetItemQuantity)
	g.Delete("/:id/items/:itemId", h.RemoveItem)
	// Pagamento recebido fora do LiveCart. Ver item_edit.go.
	g.Post("/:id/confirm-manual-payment", h.ConfirmManualPayment)
}

// List godoc
// @Summary      List orders
// @Description  Returns orders with filtering, pagination, and sorting
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string false "Search by customer handle or order ID"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(created_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        status query []string false "Filter by status"
// @Param        paymentStatus query []string false "Filter by payment status"
// @Param        eventId query string false "Filter by event (campaign) ID"
// @Param        productId query string false "Only orders containing this product"
// @Param        liveSessionId query string false "Deprecated: alias of eventId"
// @Success      200 {object} httpx.Envelope{data=ListOrdersResponse}
// @Router       /api/v1/stores/{storeId}/orders [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	input := ListOrdersInput{
		StoreID: httpx.GetStoreID(c),
		Search:  c.Query("search"),
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
		Sorting: query.Sorting{
			SortBy:    c.Query("sortBy", "created_at"),
			SortOrder: c.Query("sortOrder", "desc"),
		},
		Filters: parseOrderFilters(c),
	}

	output, err := h.service.List(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewListOrdersResponse(output))
}

// GetByID godoc
// @Summary      Get order by ID
// @Description  Returns a single order by its UUID with items and customer comments
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/orders/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	output, err := h.service.GetDetailByID(c.UserContext(), c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderDetailResponse(*output))
}

// Update godoc
// @Summary      Update an order
// @Description  Updates order status and/or payment status
// @Tags         orders
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Param        request body UpdateOrderRequest true "Order update payload"
// @Success      200 {object} httpx.Envelope{data=OrderResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/orders/{id} [patch]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	var req UpdateOrderRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	output, err := h.service.Update(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderResponse(*output))
}

// GetUpsell godoc
// @Summary      Get upsell/downsell summary for an order
// @Description  Returns the initial cart snapshot, the cart-mutation log, and the delta between initial and final subtotals.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=OrderUpsellOutput}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/upsell [get]
// @Security     BearerAuth
func (h *Handler) GetUpsell(c *fiber.Ctx) error {
	output, err := h.service.GetUpsellSummary(c.UserContext(), c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, output)
}

// GetStats godoc
// @Summary      Get order statistics
// @Description  Returns aggregated statistics for all orders in the store
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=OrderStatsResponse}
// @Router       /api/v1/stores/{storeId}/orders/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	output, err := h.service.GetStats(c.UserContext(), httpx.GetStoreID(c), c.Query("search"), parseOrderFilters(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderStatsResponse(*output))
}

// ProductBreakdown godoc
// @Summary      Orders containing a product, grouped by status
// @Description  Counts orders and units of a product per (status, paymentStatus)
// @Description  bucket — the product modal's "pedidos com este produto" view.
// @Tags         orders
// @Produce      json
// @Param        productId query string true "Product ID"
// @Router       /stores/{storeId}/orders/product-breakdown [get]
func (h *Handler) ProductBreakdown(c *fiber.Ctx) error {
	productID := c.Query("productId")
	if _, err := uuid.Parse(productID); err != nil {
		return httpx.BadRequest(c, "productId inválido")
	}
	rows, err := h.service.GetProductOrderBreakdown(c.UserContext(), httpx.GetStoreID(c), productID)
	if err != nil {
		return err
	}
	return httpx.OK(c, fiber.Map{"buckets": rows})
}

// UpdateShippingAddress godoc
// @Summary      Update an order's shipping address
// @Description  Replaces the cart's shipping_address. Blocked once the order
// @Description  is paid or has a shipment created — editing past those points
// @Description  would desynchronize the buyer's receipt and the carrier.
// @Tags         orders
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Param        request body UpdateShippingAddressRequest true "New shipping address"
// @Success      204 "No content"
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/shipping-address [patch]
// @Security     BearerAuth
func (h *Handler) UpdateShippingAddress(c *fiber.Ctx) error {
	var req UpdateShippingAddressRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	if err := h.service.UpdateShippingAddress(c.UserContext(), input); err != nil {
		return err
	}
	return httpx.NoContent(c)
}

// RegenerateCheckout godoc
// @Summary      Regenerate the checkout window for an order
// @Description  Pushes expires_at forward, resets status/payment_status, and
// @Description  clears any cached checkout url so the buyer can pay again.
// @Description  Blocked when the order is already paid or shipment has been
// @Description  created.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=RegenerateCheckoutResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/regenerate-checkout [post]
// @Security     BearerAuth
func (h *Handler) RegenerateCheckout(c *fiber.Ctx) error {
	token, expiresAt, err := h.service.RegenerateCheckout(c.UserContext(), c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewRegenerateCheckoutResponse(token, expiresAt))
}

// Cancel godoc
// @Summary      Cancelar carrinho/pedido não pago
// @Description  Cancela um pedido que ainda não foi pago: o carrinho continua
// @Description  existindo com status 'cancelled', o estoque local é devolvido, a
// @Description  reserva/pedido no ERP (Tiny) é estornada e a fila de espera do
// @Description  carrinho é encerrada. O link público passa a mostrar "carrinho
// @Description  cancelado" (e não "expirado"). Recusa com 409 quando o pedido já
// @Description  foi pago, já está cancelado/expirado, ou quando um pagamento
// @Description  está sendo finalizado neste instante — nessa corrida o pagamento
// @Description  vence e o pedido permanece pago.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/cancel [post]
// @Security     BearerAuth
func (h *Handler) Cancel(c *fiber.Ctx) error {
	id := c.Params("id")
	storeID := httpx.GetStoreID(c)

	if err := h.service.Cancel(c.UserContext(), id, storeID); err != nil {
		return err
	}

	// Devolve o pedido já atualizado para o FE trocar o estado in-place.
	output, err := h.service.GetDetailByID(c.UserContext(), id, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderDetailResponse(*output))
}

// RetryERPFinalisation godoc
// @Summary      Retry ERP order creation for a paid cart
// @Description  Re-runs the post-payment Tiny order creation for an order
// @Description  whose finalisation previously failed. Stock stays held against
// @Description  the cart between attempts, so retrying never overcommits or
// @Description  releases inventory. No-op when the cart is already finalised;
// @Description  errors when finalisation is still in 'pending' state.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/retry-erp [post]
// @Security     BearerAuth
func (h *Handler) RetryERPFinalisation(c *fiber.Ctx) error {
	id := c.Params("id")
	storeID := httpx.GetStoreID(c)

	if err := h.service.RetryERPFinalisation(c.UserContext(), id, storeID); err != nil {
		return err
	}

	// Return the refreshed order detail so the FE can swap the banner state
	// in-place without a follow-up request.
	output, err := h.service.GetDetailByID(c.UserContext(), id, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderDetailResponse(*output))
}

// SyncInvoice godoc
// @Summary      Verificar NFe na Tiny
// @Description  Pulls the NFe state from the active ERP integration and
// @Description  persists it on the order. Used by the "Verificar NFe" button
// @Description  on the order detail page when the merchant emitted the NFe
// @Description  in the ERP but the webhook didn't arrive (or hasn't been
// @Description  configured). Returns the refreshed order detail so the FE
// @Description  picks up the new erp_invoice_* fields without a follow-up
// @Description  fetch.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/sync-invoice [post]
// @Security     BearerAuth
func (h *Handler) SyncInvoice(c *fiber.Ctx) error {
	id := c.Params("id")
	storeID := httpx.GetStoreID(c)

	if err := h.service.SyncInvoice(c.UserContext(), id, storeID); err != nil {
		return err
	}

	output, err := h.service.GetDetailByID(c.UserContext(), id, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderDetailResponse(*output))
}

func parseOrderFilters(c *fiber.Ctx) OrderFilters {
	var filters OrderFilters

	statusBytes := c.Context().QueryArgs().PeekMulti("status")
	if len(statusBytes) > 0 {
		filters.Status = make([]string, len(statusBytes))
		for i, s := range statusBytes {
			filters.Status[i] = string(s)
		}
	}

	paymentStatusBytes := c.Context().QueryArgs().PeekMulti("paymentStatus")
	if len(paymentStatusBytes) > 0 {
		filters.PaymentStatus = make([]string, len(paymentStatusBytes))
		for i, ps := range paymentStatusBytes {
			filters.PaymentStatus[i] = string(ps)
		}
	}

	// eventId é o nome oficial; liveSessionId continua aceito porque é o que o
	// frontend manda hoje. Os dois sempre filtraram a MESMA coluna
	// (carts.event_id) — o nome antigo só descrevia errado (RN-19).
	if eventID := c.Query("eventId"); eventID != "" {
		filters.EventID = &eventID
	} else if legacy := c.Query("liveSessionId"); legacy != "" {
		filters.EventID = &legacy
	}

	// Vem de deep-link (/orders?product=...). Garbage viraria erro de sintaxe
	// uuid no Postgres — uuid inválido é descartado (lista sem filtro), o
	// mesmo contrato tolerante do eventId acima.
	if productID := c.Query("productId"); productID != "" {
		if _, err := uuid.Parse(productID); err == nil {
			filters.ProductID = &productID
		}
	}

	if dateFrom := c.Query("dateFrom"); dateFrom != "" {
		filters.DateFrom = &dateFrom
	}
	if dateTo := c.Query("dateTo"); dateTo != "" {
		filters.DateTo = &dateTo
	}

	if hasShipment := c.Query("hasShipment"); hasShipment != "" {
		switch hasShipment {
		case "true":
			t := true
			filters.HasShipment = &t
		case "false":
			f := false
			filters.HasShipment = &f
		}
	}

	shipmentStatusBytes := c.Context().QueryArgs().PeekMulti("shipmentStatus")
	if len(shipmentStatusBytes) > 0 {
		filters.ShipmentStatus = make([]string, len(shipmentStatusBytes))
		for i, s := range shipmentStatusBytes {
			filters.ShipmentStatus[i] = string(s)
		}
	}

	erpFinalisationBytes := c.Context().QueryArgs().PeekMulti("erpFinalisation")
	if len(erpFinalisationBytes) > 0 {
		filters.ERPFinalisation = make([]string, len(erpFinalisationBytes))
		for i, s := range erpFinalisationBytes {
			filters.ERPFinalisation[i] = string(s)
		}
	}

	if needsAttention := c.Query("needsAttention"); needsAttention != "" {
		switch needsAttention {
		case "true":
			t := true
			filters.NeedsAttention = &t
		case "false":
			f := false
			filters.NeedsAttention = &f
		}
	}

	return filters
}

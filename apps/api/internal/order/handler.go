package order

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
}

func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/orders")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
	g.Get("/:id", h.GetByID)
	g.Patch("/:id", h.Update)
}

// List godoc
// @Summary      List orders
// @Description  Returns orders with filtering, pagination, and sorting
// @Tags         orders
// @Produce      json
// @Param        search query string false "Search by customer handle or order ID"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(created_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        status query []string false "Filter by status"
// @Param        paymentStatus query []string false "Filter by payment status"
// @Param        liveSessionId query string false "Filter by live session ID"
// @Success      200 {object} httpx.Envelope{data=ListOrdersResponse}
// @Router       /api/v1/orders [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	input := ListOrdersInput{
		StoreID: storeID,
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

	output, err := h.service.List(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]OrderResponse, len(output.Orders))
	for i, o := range output.Orders {
		responses[i] = toOrderResponse(o)
	}

	return httpx.OK(c, ListOrdersResponse{
		Data:       responses,
		Pagination: query.NewPaginationResponse(output.Pagination, output.Total),
	})
}

// GetByID godoc
// @Summary      Get order by ID
// @Description  Returns a single order by its UUID with items
// @Tags         orders
// @Produce      json
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=OrderResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/orders/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toOrderResponse(*output))
}

// Update godoc
// @Summary      Update an order
// @Description  Updates order status and/or payment status
// @Tags         orders
// @Accept       json
// @Produce      json
// @Param        id path string true "Order UUID"
// @Param        request body UpdateOrderRequest true "Order update payload"
// @Success      200 {object} httpx.Envelope{data=OrderResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/orders/{id} [patch]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	var req UpdateOrderRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.Update(c.Context(), UpdateOrderInput{
		ID:            id,
		StoreID:       storeID,
		Status:        req.Status,
		PaymentStatus: req.PaymentStatus,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toOrderResponse(*output))
}

// GetStats godoc
// @Summary      Get order statistics
// @Description  Returns aggregated statistics for all orders in the store
// @Tags         orders
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=OrderStatsResponse}
// @Router       /api/v1/orders/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetStats(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, OrderStatsResponse{
		TotalOrders:   output.TotalOrders,
		PendingOrders: output.PendingOrders,
		TotalRevenue:  output.TotalRevenue,
		AvgTicket:     output.AvgTicket,
	})
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

	if liveSessionID := c.Query("liveSessionId"); liveSessionID != "" {
		filters.LiveSessionID = &liveSessionID
	}

	if dateFrom := c.Query("dateFrom"); dateFrom != "" {
		filters.DateFrom = &dateFrom
	}
	if dateTo := c.Query("dateTo"); dateTo != "" {
		filters.DateTo = &dateTo
	}

	return filters
}

func toOrderResponse(o OrderOutput) OrderResponse {
	items := make([]OrderItemResponse, len(o.Items))
	for i, item := range o.Items {
		items[i] = OrderItemResponse{
			ID:           item.ID,
			ProductID:    item.ProductID,
			ProductName:  item.ProductName,
			ProductImage: item.ProductImage,
			Keyword:      item.Keyword,
			Size:         item.Size,
			Quantity:     item.Quantity,
			UnitPrice:    item.UnitPrice,
			TotalPrice:   item.TotalPrice,
		}
	}

	return OrderResponse{
		ID:             o.ID,
		LiveSessionID:  o.LiveSessionID,
		LiveTitle:      o.LiveTitle,
		LivePlatform:   o.LivePlatform,
		CustomerHandle: o.CustomerHandle,
		CustomerID:     o.CustomerID,
		Status:         o.Status,
		PaymentStatus:  o.PaymentStatus,
		Items:          items,
		TotalItems:     o.TotalItems,
		TotalAmount:    o.TotalAmount,
		PaidAt:         o.PaidAt,
		CreatedAt:      o.CreatedAt,
		ExpiresAt:      o.ExpiresAt,
	}
}

package customer

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
	g := router.Group("/customers")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
	// /blocks routes registered before /:id so the literal segment wins over
	// the UUID-shaped param.
	h.RegisterBlockRoutes(g)
	// /vips antes de /:id, mesmo motivo dos /blocks (literal vence o UUID).
	h.RegisterVipRoutes(g)
	g.Get("/:id", h.GetByID)
	g.Get("/:id/orders", h.ListOrders)
}

// List godoc
// @Summary      List customers
// @Description  Returns customers with filtering, pagination, and sorting
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string false "Search by handle"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(last_order_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        hasOrders query bool false "Filter customers with orders"
// @Param        orderCountMin query int false "Minimum order count"
// @Param        orderCountMax query int false "Maximum order count"
// @Success      200 {object} httpx.Envelope{data=ListCustomersResponse}
// @Router       /api/v1/stores/{storeId}/customers [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	input := ListCustomersInput{
		StoreID: httpx.GetStoreID(c),
		Search:  c.Query("search"),
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
		Sorting: query.Sorting{
			SortBy:    c.Query("sortBy", "last_order_at"),
			SortOrder: c.Query("sortOrder", "desc"),
		},
		Filters: parseCustomerFilters(c),
	}

	customers, pagination, total, err := h.service.List(c.UserContext(), input)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewListCustomersResponse(customers, pagination, total))
}

// GetByID godoc
// @Summary      Get customer by ID
// @Description  Returns a single customer by their UUID
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Customer UUID"
// @Success      200 {object} httpx.Envelope{data=CustomerResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/customers/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return httpx.ErrBadRequest("invalid customer id")
	}

	cust, err := h.service.GetByID(c.UserContext(), id)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewCustomerResponse(cust))
}

// GetStats godoc
// @Summary      Get customer statistics
// @Description  Returns aggregated statistics for all customers in the store
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=CustomerStatsResponse}
// @Router       /api/v1/stores/{storeId}/customers/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	stats, err := h.service.GetStats(c.UserContext(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}

	return httpx.OK(c, NewCustomerStatsResponse(stats))
}

// ListOrders godoc
// @Summary      List orders for a customer
// @Description  Returns the most recent carts (paid + pending) for the given customer
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Customer UUID"
// @Param        limit query int false "Items per page" default(20)
// @Param        offset query int false "Offset" default(0)
// @Success      200 {object} httpx.Envelope{data=[]CustomerOrderResponse}
// @Router       /api/v1/stores/{storeId}/customers/{id}/orders [get]
// @Security     BearerAuth
func (h *Handler) ListOrders(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return httpx.ErrBadRequest("invalid customer id")
	}

	limit := int32(c.QueryInt("limit", 20))
	offset := int32(c.QueryInt("offset", 0))

	orders, err := h.service.ListOrders(c.UserContext(), id, limit, offset)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewListCustomerOrdersResponse(orders))
}

func parseCustomerFilters(c *fiber.Ctx) CustomerFilters {
	var filters CustomerFilters

	if hasOrders := c.Query("hasOrders"); hasOrders != "" {
		val := hasOrders == "true"
		filters.HasOrders = &val
	}

	if orderCountMin := c.QueryInt("orderCountMin", -1); orderCountMin >= 0 {
		filters.OrderCountMin = &orderCountMin
	}
	if orderCountMax := c.QueryInt("orderCountMax", -1); orderCountMax >= 0 {
		filters.OrderCountMax = &orderCountMax
	}
	if totalSpentMin := c.QueryInt("totalSpentMin", -1); totalSpentMin >= 0 {
		filters.TotalSpentMin = &totalSpentMin
	}
	if totalSpentMax := c.QueryInt("totalSpentMax", -1); totalSpentMax >= 0 {
		filters.TotalSpentMax = &totalSpentMax
	}

	return filters
}

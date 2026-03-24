package customer

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
	g := router.Group("/customers")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
	g.Get("/:id", h.GetByID)
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
	storeID := c.Locals("store_id").(string)

	input := ListCustomersInput{
		StoreID: storeID,
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

	output, err := h.service.List(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]CustomerResponse, len(output.Customers))
	for i, o := range output.Customers {
		responses[i] = toCustomerResponse(o)
	}

	return httpx.OK(c, ListCustomersResponse{
		Data:       responses,
		Pagination: query.NewPaginationResponse(output.Pagination, output.Total),
	})
}

// GetByID godoc
// @Summary      Get customer by ID
// @Description  Returns a single customer by their platform user ID
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Customer platform user ID"
// @Success      200 {object} httpx.Envelope{data=CustomerResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/customers/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetByID(c.Context(), storeID, id)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toCustomerResponse(*output))
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
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetStats(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, CustomerStatsResponse{
		TotalCustomers:      output.TotalCustomers,
		ActiveCustomers:     output.ActiveCustomers,
		AvgSpentPerCustomer: output.AvgSpentPerCustomer,
	})
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

func toCustomerResponse(o CustomerOutput) CustomerResponse {
	return CustomerResponse{
		ID:           o.ID,
		Handle:       o.Handle,
		TotalOrders:  o.TotalOrders,
		TotalSpent:   o.TotalSpent,
		LastOrderAt:  o.LastOrderAt,
		FirstOrderAt: o.FirstOrderAt,
	}
}

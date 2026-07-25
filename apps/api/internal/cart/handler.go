package cart

import (
	"github.com/gofiber/fiber/v2"

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
	g := router.Group("/carts")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
}

// List godoc
// @Summary      List pending carts
// @Description  Returns carts without a paid order (active/checkout/expired) for recovery workflows
// @Tags         carts
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string false "Search by handle, short_id, customer name or phone"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(created_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        status query []string false "Filter by cart status (active, checkout, expired)"
// @Success      200 {object} httpx.Envelope{data=ListCartsResponse}
// @Router       /api/v1/stores/{storeId}/carts [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	statusBytes := c.Context().QueryArgs().PeekMulti("status")
	statuses := make([]string, len(statusBytes))
	for i, s := range statusBytes {
		statuses[i] = string(s)
	}

	input := ListCartsInput{
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
		Filters: CartFilters{Status: statuses},
	}

	output, err := h.service.List(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewListCartsResponse(output))
}

// GetStats godoc
// @Summary      Cart recovery metrics
// @Description  Aggregated stats for pending carts: open count/value, avg ticket, recoverable count
// @Tags         carts
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=CartStatsResponse}
// @Router       /api/v1/stores/{storeId}/carts/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	output, err := h.service.GetStats(c.UserContext(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewCartStatsResponse(*output))
}

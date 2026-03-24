package dashboard

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/dashboard")
	g.Get("/stats", h.GetStats)
	g.Get("/chart", h.GetMonthlyRevenue)
	g.Get("/top-products", h.GetTopProducts)
}

// GetStats godoc
// @Summary      Get dashboard statistics
// @Description  Returns main dashboard metrics (revenue, orders, products, lives)
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=DashboardStatsResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetStats(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, DashboardStatsResponse{
		TotalRevenue:   output.TotalRevenue,
		TotalOrders:    output.TotalOrders,
		ActiveProducts: output.ActiveProducts,
		TotalLives:     output.TotalLives,
	})
}

// GetMonthlyRevenue godoc
// @Summary      Get monthly revenue chart data
// @Description  Returns monthly revenue data for the current year
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=MonthlyRevenueResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/chart [get]
// @Security     BearerAuth
func (h *Handler) GetMonthlyRevenue(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetMonthlyRevenue(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	items := make([]MonthlyRevenueItem, len(output.Items))
	for i, row := range output.Items {
		items[i] = MonthlyRevenueItem{
			Month:    row.Month,
			MonthNum: row.MonthNum,
			Revenue:  row.Revenue,
		}
	}

	return httpx.OK(c, MonthlyRevenueResponse{Data: items})
}

// GetTopProducts godoc
// @Summary      Get top selling products
// @Description  Returns the top 5 best selling products
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=TopProductsResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/top-products [get]
// @Security     BearerAuth
func (h *Handler) GetTopProducts(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetTopProducts(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	products := make([]TopProductItem, len(output.Products))
	for i, row := range output.Products {
		products[i] = TopProductItem{
			ID:           row.ID,
			Name:         row.Name,
			Keyword:      row.Keyword,
			TotalSold:    row.TotalSold,
			TotalRevenue: row.TotalRevenue,
		}
	}

	return httpx.OK(c, TopProductsResponse{Data: products})
}

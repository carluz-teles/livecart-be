package dashboard

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
}

func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/dashboard")
	g.Get("/stats", h.GetStats)
	g.Get("/chart", h.GetMonthlyRevenue)
	g.Get("/top-products", h.GetTopProducts)
	g.Get("/top-buyers", h.GetTopBuyers)
	g.Get("/product-sales", h.GetProductSales)
	g.Get("/revenue-by-payment", h.GetRevenueByPaymentMethod)

	// Analytics endpoints
	analytics := g.Group("/analytics")
	analytics.Get("/events", h.GetEventsWithRevenue)
	analytics.Get("/funnel", h.GetAggregatedFunnel)
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

// =============================================================================
// ANALYTICS - Revenue Attribution
// =============================================================================

// GetEventsWithRevenue godoc
// @Summary      Get events with revenue metrics
// @Description  Returns all events with their GMV and conversion metrics
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        limit query int false "Maximum number of events to return" default(20)
// @Success      200 {object} httpx.Envelope{data=EventsWithRevenueResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/analytics/events [get]
// @Security     BearerAuth
func (h *Handler) GetEventsWithRevenue(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	limit := c.QueryInt("limit", 20)
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	events, err := h.service.GetEventsWithRevenue(c.Context(), storeID, limit)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	items := make([]EventWithRevenueItem, len(events))
	for i, e := range events {
		// Calculate conversion rate: paidCarts / totalComments * 100
		var conversionRate float64
		if e.TotalComments > 0 {
			conversionRate = float64(e.PaidCarts) / float64(e.TotalComments) * 100
		}

		items[i] = EventWithRevenueItem{
			ID:               e.ID,
			Title:            e.Title,
			Status:           e.Status,
			CreatedAt:        e.CreatedAt,
			TotalComments:    e.TotalComments,
			TotalCarts:       e.TotalCarts,
			PaidCarts:        e.PaidCarts,
			ConfirmedRevenue: e.ConfirmedRevenue,
			ConversionRate:   conversionRate,
		}
	}

	return httpx.OK(c, EventsWithRevenueResponse{Data: items})
}

// GetAggregatedFunnel godoc
// @Summary      Get aggregated funnel metrics
// @Description  Returns aggregated conversion funnel metrics for the store
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        days query int false "Number of days to analyze" default(30)
// @Success      200 {object} httpx.Envelope{data=AggregatedFunnelResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/analytics/funnel [get]
// @Security     BearerAuth
func (h *Handler) GetAggregatedFunnel(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	days := c.QueryInt("days", 30)
	if days <= 0 || days > 365 {
		days = 30
	}

	funnel, err := h.service.GetAggregatedFunnel(c.Context(), storeID, days)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Calculate conversion rates
	var commentsToCartsRate, cartsToCheckoutRate, checkoutToPaidRate, overallRate float64

	if funnel.TotalComments > 0 {
		commentsToCartsRate = float64(funnel.TotalCarts) / float64(funnel.TotalComments) * 100
		overallRate = float64(funnel.PaidCarts) / float64(funnel.TotalComments) * 100
	}
	if funnel.TotalCarts > 0 {
		cartsToCheckoutRate = float64(funnel.CheckoutCarts) / float64(funnel.TotalCarts) * 100
	}
	if funnel.CheckoutCarts > 0 {
		checkoutToPaidRate = float64(funnel.PaidCarts) / float64(funnel.CheckoutCarts) * 100
	}

	return httpx.OK(c, AggregatedFunnelResponse{
		TotalComments:         funnel.TotalComments,
		TotalCarts:            funnel.TotalCarts,
		CheckoutCarts:         funnel.CheckoutCarts,
		PaidCarts:             funnel.PaidCarts,
		ConfirmedRevenue:      funnel.ConfirmedRevenue,
		AverageTicket:         funnel.AverageTicket,
		CommentsToCartsRate:   commentsToCartsRate,
		CartsToCheckoutRate:   cartsToCheckoutRate,
		CheckoutToPaidRate:    checkoutToPaidRate,
		OverallConversionRate: overallRate,
	})
}

// =============================================================================
// TOP BUYERS
// =============================================================================

// GetTopBuyers godoc
// @Summary      Get top buyers by total spent
// @Description  Returns the top 5 buyers sorted by total amount spent
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=TopBuyersResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/top-buyers [get]
// @Security     BearerAuth
func (h *Handler) GetTopBuyers(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetTopBuyers(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	buyers := make([]TopBuyerItem, len(output.Buyers))
	for i, row := range output.Buyers {
		buyers[i] = TopBuyerItem{
			ID:             row.ID,
			Handle:         row.Handle,
			TotalOrders:    row.TotalOrders,
			TotalSpent:     row.TotalSpent,
			LastPurchaseAt: row.LastPurchaseAt,
		}
	}

	return httpx.OK(c, TopBuyersResponse{Data: buyers})
}

// =============================================================================
// PRODUCT SALES (Stacked Bar Chart)
// =============================================================================

// GetProductSales godoc
// @Summary      Get monthly sales by product
// @Description  Returns monthly sales data by product for stacked bar chart
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=ProductSalesResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/product-sales [get]
// @Security     BearerAuth
func (h *Handler) GetProductSales(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetProductSales(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	products := make([]ProductSalesProduct, len(output.Products))
	for i, p := range output.Products {
		products[i] = ProductSalesProduct{
			ID:      p.ID,
			Name:    p.Name,
			Keyword: p.Keyword,
		}
	}

	data := make([]ProductSalesDataPoint, len(output.Data))
	for i, d := range output.Data {
		data[i] = ProductSalesDataPoint{
			Month:    d.Month,
			MonthNum: d.MonthNum,
			Values:   d.Values,
		}
	}

	return httpx.OK(c, ProductSalesResponse{
		Products: products,
		Data:     data,
	})
}

// =============================================================================
// REVENUE BY PAYMENT METHOD (Pie Chart)
// =============================================================================

// paymentMethodLabels maps payment method codes to display labels
var paymentMethodLabels = map[string]string{
	"pix":         "PIX",
	"credit_card": "Cartão de Crédito",
	"debit_card":  "Cartão de Débito",
	"boleto":      "Boleto",
	"other":       "Outros",
}

// GetRevenueByPaymentMethod godoc
// @Summary      Get revenue by payment method
// @Description  Returns revenue distribution by payment method for pie chart
// @Tags         dashboard
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=RevenueByPaymentResponse}
// @Router       /api/v1/stores/{storeId}/dashboard/revenue-by-payment [get]
// @Security     BearerAuth
func (h *Handler) GetRevenueByPaymentMethod(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetRevenueByPaymentMethod(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	items := make([]RevenueByPaymentItem, len(output.Items))
	for i, row := range output.Items {
		label, ok := paymentMethodLabels[row.PaymentMethod]
		if !ok {
			label = row.PaymentMethod
		}

		items[i] = RevenueByPaymentItem{
			PaymentMethod: row.PaymentMethod,
			Label:         label,
			Revenue:       row.Revenue,
			Count:         row.Count,
		}
	}

	return httpx.OK(c, RevenueByPaymentResponse{Data: items})
}

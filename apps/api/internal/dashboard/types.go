package dashboard

// Handler layer - Response types
type DashboardStatsResponse struct {
	TotalRevenue   int64 `json:"totalRevenue"`
	TotalOrders    int   `json:"totalOrders"`
	ActiveProducts int   `json:"activeProducts"`
	TotalLives     int   `json:"totalLives"`
}

type MonthlyRevenueResponse struct {
	Data []MonthlyRevenueItem `json:"data"`
}

type MonthlyRevenueItem struct {
	Month    string `json:"month"`
	MonthNum int    `json:"monthNum"`
	Revenue  int64  `json:"revenue"`
}

type TopProductsResponse struct {
	Data []TopProductItem `json:"data"`
}

type TopProductItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Keyword      string `json:"keyword"`
	TotalSold    int    `json:"totalSold"`
	TotalRevenue int64  `json:"totalRevenue"`
}

// Service layer
type DashboardStatsOutput struct {
	TotalRevenue   int64
	TotalOrders    int
	ActiveProducts int
	TotalLives     int
}

type MonthlyRevenueOutput struct {
	Items []MonthlyRevenueRow
}

type TopProductsOutput struct {
	Products []TopProductRow
}

// Repository layer
type DashboardStatsRow struct {
	TotalRevenue   int64
	TotalOrders    int
	ActiveProducts int
	TotalLives     int
}

type MonthlyRevenueRow struct {
	Month    string
	MonthNum int
	Revenue  int64
}

type TopProductRow struct {
	ID           string
	Name         string
	Keyword      string
	TotalSold    int
	TotalRevenue int64
}

// =============================================================================
// ANALYTICS - Revenue Attribution
// =============================================================================

// Handler layer - Analytics Response types
type EventsWithRevenueResponse struct {
	Data []EventWithRevenueItem `json:"data"`
}

type EventWithRevenueItem struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Status           string `json:"status"`
	CreatedAt        string `json:"createdAt"`
	TotalComments    int    `json:"totalComments"`
	TotalCarts       int    `json:"totalCarts"`
	PaidCarts        int    `json:"paidCarts"`
	ConfirmedRevenue int64  `json:"confirmedRevenue"`
	ConversionRate   float64 `json:"conversionRate"` // Calculated: paidCarts/totalComments * 100
}

type AggregatedFunnelResponse struct {
	TotalComments    int     `json:"totalComments"`
	TotalCarts       int     `json:"totalCarts"`
	CheckoutCarts    int     `json:"checkoutCarts"`
	PaidCarts        int     `json:"paidCarts"`
	ConfirmedRevenue int64   `json:"confirmedRevenue"`
	AverageTicket    int64   `json:"averageTicket"`
	// Conversion rates (percentages)
	CommentsToCartsRate  float64 `json:"commentsToCartsRate"`
	CartsToCheckoutRate  float64 `json:"cartsToCheckoutRate"`
	CheckoutToPaidRate   float64 `json:"checkoutToPaidRate"`
	OverallConversionRate float64 `json:"overallConversionRate"`
}

// Service layer - Analytics
type EventWithRevenueOutput struct {
	ID               string
	Title            string
	Status           string
	CreatedAt        string
	TotalComments    int
	TotalCarts       int
	PaidCarts        int
	ConfirmedRevenue int64
}

type AggregatedFunnelOutput struct {
	TotalComments    int
	TotalCarts       int
	CheckoutCarts    int
	PaidCarts        int
	ConfirmedRevenue int64
	AverageTicket    int64
}

// Repository layer - Analytics
type EventWithRevenueRow struct {
	ID               string
	Title            string
	Status           string
	CreatedAt        string
	TotalComments    int
	TotalCarts       int
	PaidCarts        int
	ConfirmedRevenue int64
}

type AggregatedFunnelRow struct {
	TotalComments    int
	TotalCarts       int
	CheckoutCarts    int
	PaidCarts        int
	ConfirmedRevenue int64
	AverageTicket    int64
}

// =============================================================================
// TOP BUYERS
// =============================================================================

// Handler layer - Top Buyers Response
type TopBuyersResponse struct {
	Data []TopBuyerItem `json:"data"`
}

type TopBuyerItem struct {
	ID             string `json:"id"`
	Handle         string `json:"handle"`
	TotalOrders    int    `json:"totalOrders"`
	TotalSpent     int64  `json:"totalSpent"`
	LastPurchaseAt string `json:"lastPurchaseAt"`
}

// Service layer - Top Buyers
type TopBuyersOutput struct {
	Buyers []TopBuyerRow
}

// Repository layer - Top Buyers
type TopBuyerRow struct {
	ID             string
	Handle         string
	TotalOrders    int
	TotalSpent     int64
	LastPurchaseAt string
}

// =============================================================================
// PRODUCT SALES (Stacked Bar Chart)
// =============================================================================

// Handler layer - Product Sales Response
type ProductSalesResponse struct {
	Products []ProductSalesProduct   `json:"products"`
	Data     []ProductSalesDataPoint `json:"data"`
}

type ProductSalesProduct struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Keyword string `json:"keyword"`
}

type ProductSalesDataPoint struct {
	Month    string           `json:"month"`
	MonthNum int              `json:"monthNum"`
	Values   map[string]int64 `json:"values"`
}

// Service layer - Product Sales
type ProductSalesOutput struct {
	Products []ProductSalesProductRow
	Data     []ProductSalesDataRow
}

type ProductSalesProductRow struct {
	ID      string
	Name    string
	Keyword string
}

type ProductSalesDataRow struct {
	Month    string
	MonthNum int
	Values   map[string]int64
}

// =============================================================================
// REVENUE BY PAYMENT METHOD (Pie Chart)
// =============================================================================

// Handler layer - Revenue by Payment Method Response
type RevenueByPaymentResponse struct {
	Data []RevenueByPaymentItem `json:"data"`
}

type RevenueByPaymentItem struct {
	PaymentMethod string `json:"paymentMethod"`
	Label         string `json:"label"`
	Revenue       int64  `json:"revenue"`
	Count         int    `json:"count"`
}

// Service layer - Revenue by Payment Method
type RevenueByPaymentOutput struct {
	Items []RevenueByPaymentRow
}

// Repository layer - Revenue by Payment Method
type RevenueByPaymentRow struct {
	PaymentMethod string
	Revenue       int64
	Count         int
}

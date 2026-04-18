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

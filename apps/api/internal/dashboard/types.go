package dashboard

// Handler layer - Response types
type DashboardStatsResponse struct {
	TotalRevenue   int64 `json:"total_revenue"`
	TotalOrders    int   `json:"total_orders"`
	ActiveProducts int   `json:"active_products"`
	TotalLives     int   `json:"total_lives"`
}

type MonthlyRevenueResponse struct {
	Data []MonthlyRevenueItem `json:"data"`
}

type MonthlyRevenueItem struct {
	Month    string `json:"month"`
	MonthNum int    `json:"month_num"`
	Revenue  int64  `json:"revenue"`
}

type TopProductsResponse struct {
	Data []TopProductItem `json:"data"`
}

type TopProductItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Keyword      string `json:"keyword"`
	TotalSold    int    `json:"total_sold"`
	TotalRevenue int64  `json:"total_revenue"`
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

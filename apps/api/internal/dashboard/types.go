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

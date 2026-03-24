package customer

import (
	"time"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type CustomerFilters struct {
	HasOrders      *bool `query:"hasOrders"`
	OrderCountMin  *int  `query:"orderCountMin"`
	OrderCountMax  *int  `query:"orderCountMax"`
	TotalSpentMin  *int  `query:"totalSpentMin"`
	TotalSpentMax  *int  `query:"totalSpentMax"`
}

// Handler layer - Request/Response types
type CustomerResponse struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	TotalOrders  int        `json:"totalOrders"`
	TotalSpent   int64      `json:"totalSpent"`
	LastOrderAt  *time.Time `json:"lastOrderAt"`
	FirstOrderAt *time.Time `json:"firstOrderAt"`
}

type ListCustomersResponse struct {
	Data       []CustomerResponse       `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type CustomerStatsResponse struct {
	TotalCustomers      int   `json:"totalCustomers"`
	ActiveCustomers     int   `json:"activeCustomers"`
	AvgSpentPerCustomer int64 `json:"avgSpentPerCustomer"`
}

// Service layer
type ListCustomersInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CustomerFilters
}

type ListCustomersOutput struct {
	Customers  []CustomerOutput
	Total      int
	Pagination query.Pagination
}

type CustomerOutput struct {
	ID           string
	Handle       string
	TotalOrders  int
	TotalSpent   int64
	LastOrderAt  *time.Time
	FirstOrderAt *time.Time
}

type CustomerStatsOutput struct {
	TotalCustomers      int
	ActiveCustomers     int
	AvgSpentPerCustomer int64
}

// Repository layer
type ListCustomersParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CustomerFilters
}

type ListCustomersResult struct {
	Customers []CustomerRow
	Total     int
}

type CustomerRow struct {
	ID           string
	Handle       string
	TotalOrders  int
	TotalSpent   int64
	LastOrderAt  *time.Time
	FirstOrderAt *time.Time
}

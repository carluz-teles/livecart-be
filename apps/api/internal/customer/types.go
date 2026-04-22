package customer

import (
	"time"

	"github.com/google/uuid"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type CustomerFilters struct {
	HasOrders     *bool `query:"hasOrders"`
	OrderCountMin *int  `query:"orderCountMin"`
	OrderCountMax *int  `query:"orderCountMax"`
	TotalSpentMin *int  `query:"totalSpentMin"`
	TotalSpentMax *int  `query:"totalSpentMax"`
}

// Handler layer - Request/Response types
type CustomerResponse struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	Email        *string    `json:"email,omitempty"`
	Phone        *string    `json:"phone,omitempty"`
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

// Service layer - Input types
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
	Email        *string
	Phone        *string
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

// UpsertCustomerInput is used to create or update a customer
type UpsertCustomerInput struct {
	StoreID        uuid.UUID
	PlatformUserID string
	PlatformHandle string
	Email          *string
	Phone          *string
}

// UpdateCustomerInput is used to update customer fields
type UpdateCustomerInput struct {
	Handle *string
	Email  *string
	Phone  *string
}

// Repository layer types
type ListCustomersParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CustomerFilters
}

type ListCustomersResult struct {
	Customers []CustomerWithStatsRow
	Total     int
}

// CustomerRow represents a customer from the database (basic info)
type CustomerRow struct {
	ID             string
	PlatformUserID string
	Handle         string
	Email          *string
	Phone          *string
	LastOrderAt    *time.Time
	FirstOrderAt   *time.Time
}

// CustomerWithStatsRow includes aggregated order stats
type CustomerWithStatsRow struct {
	ID             string
	PlatformUserID string
	Handle         string
	Email          *string
	Phone          *string
	TotalOrders    int
	TotalSpent     int64
	LastOrderAt    *time.Time
	FirstOrderAt   *time.Time
}

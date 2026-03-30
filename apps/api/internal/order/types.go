package order

import (
	"time"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type OrderFilters struct {
	Status        []string `query:"status"`        // pending, checkout, completed, expired
	PaymentStatus []string `query:"paymentStatus"` // pending, paid, failed, refunded
	LiveSessionID *string  `query:"liveSessionId"`
	DateFrom      *string  `query:"dateFrom"`
	DateTo        *string  `query:"dateTo"`
}

// Handler layer - Request/Response types
type UpdateOrderRequest struct {
	Status        *string `json:"status" validate:"omitempty,oneof=pending checkout completed expired"`
	PaymentStatus *string `json:"paymentStatus" validate:"omitempty,oneof=pending paid failed refunded"`
}

type OrderItemResponse struct {
	ID           string  `json:"id"`
	ProductID    string  `json:"productId"`
	ProductName  string  `json:"productName"`
	ProductImage *string `json:"productImage"`
	Keyword      string  `json:"keyword"`
	Size         *string `json:"size"`
	Quantity     int     `json:"quantity"`
	UnitPrice    int64   `json:"unitPrice"`
	TotalPrice   int64   `json:"totalPrice"`
}

type OrderResponse struct {
	ID             string              `json:"id"`
	LiveSessionID  string              `json:"liveSessionId"`
	LiveTitle      string              `json:"liveTitle"`
	LivePlatform   string              `json:"livePlatform"`
	CustomerHandle string              `json:"customerHandle"`
	CustomerID     string              `json:"customerId"`
	Status         string              `json:"status"`
	PaymentStatus  string              `json:"paymentStatus"`
	Items          []OrderItemResponse `json:"items"`
	TotalItems     int                 `json:"totalItems"`
	TotalAmount    int64               `json:"totalAmount"`
	PaidAt         *time.Time          `json:"paidAt"`
	CreatedAt      time.Time           `json:"createdAt"`
	ExpiresAt      *time.Time          `json:"expiresAt"`
}

type ListOrdersResponse struct {
	Data       []OrderResponse          `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type OrderStatsResponse struct {
	TotalOrders   int   `json:"totalOrders"`
	PendingOrders int   `json:"pendingOrders"`
	TotalRevenue  int64 `json:"totalRevenue"`
	AvgTicket     int64 `json:"avgTicket"`
}

// Service layer
type ListOrdersInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    OrderFilters
}

type ListOrdersOutput struct {
	Orders     []OrderOutput
	Total      int
	Pagination query.Pagination
}

type OrderOutput struct {
	ID             string
	LiveSessionID  string
	LiveTitle      string
	LivePlatform   string
	CustomerHandle string
	CustomerID     string
	Status         string
	PaymentStatus  string
	Items          []OrderItemOutput
	TotalItems     int
	TotalAmount    int64
	PaidAt         *time.Time
	CreatedAt      time.Time
	ExpiresAt      *time.Time
}

type OrderItemOutput struct {
	ID           string
	ProductID    string
	ProductName  string
	ProductImage *string
	Keyword      string
	Size         *string
	Quantity     int
	UnitPrice    int64
	TotalPrice   int64
}

type UpdateOrderInput struct {
	ID            string
	StoreID       string
	Status        *string
	PaymentStatus *string
}

type OrderStatsOutput struct {
	TotalOrders   int
	PendingOrders int
	TotalRevenue  int64
	AvgTicket     int64
}

// Repository layer
type ListOrdersParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    OrderFilters
}

type ListOrdersResult struct {
	Orders []OrderRow
	Total  int
}

type OrderRow struct {
	ID             string
	EventID        string
	PlatformUserID string
	PlatformHandle string
	Token          string
	Status         string
	PaymentStatus  string
	PaidAt         *time.Time
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	LiveTitle      string
	LivePlatform   string
	TotalAmount    int64
	TotalItems     int
}

type OrderItemRow struct {
	ID           string
	CartID       string
	ProductID    string
	Size         *string
	Quantity     int
	UnitPrice    int64
	ProductName  string
	ProductImage *string
	ProductKeyword string
}

type OrderDetailRow struct {
	ID             string
	EventID        string
	PlatformUserID string
	PlatformHandle string
	Token          string
	Status         string
	PaymentStatus  string
	PaidAt         *time.Time
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	LiveTitle      string
	LivePlatform   string
	StoreID        string
}

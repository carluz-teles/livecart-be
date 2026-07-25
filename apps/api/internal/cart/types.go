package cart

import (
	"time"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters

type CartFilters struct {
	Status []string `query:"status"` // active, checkout, expired
}

// Handler layer - Request/Response

type CartResponse struct {
	ID             string     `json:"id"`
	ShortID        int        `json:"shortId"`
	PlatformHandle string     `json:"platformHandle"`
	CustomerName   string     `json:"customerName"`
	CustomerPhone  string     `json:"customerPhone"`
	TotalCents     int64      `json:"totalCents"`
	Status         string     `json:"status"`
	ExpiresAt      *time.Time `json:"expiresAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

type ListCartsResponse struct {
	Data       []CartResponse           `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type CartStatsResponse struct {
	OpenCarts        int   `json:"openCarts"`
	OpenValueCents   int64 `json:"openValueCents"`
	AvgTicketCents   int64 `json:"avgTicketCents"`
	RecoverableCarts int   `json:"recoverableCarts"`
}

// Service layer

type ListCartsInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CartFilters
}

type ListCartsOutput struct {
	Carts      []CartOutput
	Total      int
	Pagination query.Pagination
}

type CartOutput struct {
	ID             string
	ShortID        int
	PlatformHandle string
	CustomerName   string
	CustomerPhone  string
	TotalCents     int64
	Status         string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
}

type CartStatsOutput struct {
	OpenCarts        int
	OpenValueCents   int64
	AvgTicketCents   int64
	RecoverableCarts int
}

// Repository layer

type ListCartsParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CartFilters
}

type ListCartsResult struct {
	Carts []CartRow
	Total int
}

type CartRow struct {
	ID             string
	ShortID        int
	PlatformHandle string
	CustomerName   string
	CustomerPhone  string
	TotalCents     int64
	Status         string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
}

type CartStatsRow struct {
	OpenCarts        int
	OpenValueCents   int64
	AvgTicketCents   int64
	RecoverableCarts int
}

// Response mappers

func NewCartResponse(c CartOutput) CartResponse {
	return CartResponse{
		ID:             c.ID,
		ShortID:        c.ShortID,
		PlatformHandle: c.PlatformHandle,
		CustomerName:   c.CustomerName,
		CustomerPhone:  c.CustomerPhone,
		TotalCents:     c.TotalCents,
		Status:         c.Status,
		ExpiresAt:      c.ExpiresAt,
		CreatedAt:      c.CreatedAt,
	}
}

func NewListCartsResponse(out ListCartsOutput) ListCartsResponse {
	responses := make([]CartResponse, len(out.Carts))
	for i, c := range out.Carts {
		responses[i] = NewCartResponse(c)
	}
	return ListCartsResponse{
		Data:       responses,
		Pagination: query.NewPaginationResponse(out.Pagination, out.Total),
	}
}

func NewCartStatsResponse(o CartStatsOutput) CartStatsResponse {
	return CartStatsResponse{
		OpenCarts:        o.OpenCarts,
		OpenValueCents:   o.OpenValueCents,
		AvgTicketCents:   o.AvgTicketCents,
		RecoverableCarts: o.RecoverableCarts,
	}
}

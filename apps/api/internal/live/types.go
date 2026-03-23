package live

import (
	"time"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type LiveFilters struct {
	Status   []string `query:"status"`   // scheduled, live, ended, cancelled
	Platform []string `query:"platform"` // instagram, tiktok, youtube, facebook
	DateFrom *string  `query:"dateFrom"`
	DateTo   *string  `query:"dateTo"`
}

// Handler layer - Request/Response types
type CreateLiveRequest struct {
	Title          string `json:"title" validate:"required,min=1,max=200"`
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platform_live_id" validate:"required"`
}

type CreateLiveResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Platform  string    `json:"platform"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type UpdateLiveRequest struct {
	Title          string `json:"title" validate:"required,min=1,max=200"`
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platform_live_id" validate:"required"`
}

type LiveResponse struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Platform       string     `json:"platform"`
	PlatformLiveID string     `json:"platform_live_id"`
	Status         string     `json:"status"`
	StartedAt      *time.Time `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at"`
	TotalComments  int        `json:"total_comments"`
	TotalOrders    int        `json:"total_orders"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type ListLivesResponse struct {
	Data       []LiveResponse           `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type LiveStatsResponse struct {
	TotalLives  int `json:"total_lives"`
	ActiveLives int `json:"active_lives"`
	TotalOrders int `json:"total_orders"`
}

// Service layer
type CreateLiveInput struct {
	StoreID        string
	Title          string
	Platform       string
	PlatformLiveID string
}

type CreateLiveOutput struct {
	ID        string
	Title     string
	Platform  string
	Status    string
	CreatedAt time.Time
}

type UpdateLiveInput struct {
	ID             string
	StoreID        string
	Title          string
	Platform       string
	PlatformLiveID string
}

type ListLivesInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    LiveFilters
}

type ListLivesOutput struct {
	Lives      []LiveOutput
	Total      int
	Pagination query.Pagination
}

type LiveOutput struct {
	ID             string
	Title          string
	Platform       string
	PlatformLiveID string
	Status         string
	StartedAt      *time.Time
	EndedAt        *time.Time
	TotalComments  int
	TotalOrders    int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type LiveStatsOutput struct {
	TotalLives  int
	ActiveLives int
	TotalOrders int
}

// Repository layer
type CreateLiveParams struct {
	StoreID        string
	Title          string
	Platform       string
	PlatformLiveID string
	Status         string
}

type UpdateLiveParams struct {
	ID             string
	StoreID        string
	Title          string
	Platform       string
	PlatformLiveID string
}

type ListLivesParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    LiveFilters
}

type ListLivesResult struct {
	Lives []LiveRow
	Total int
}

type LiveRow struct {
	ID             string
	StoreID        string
	Title          string
	Platform       string
	PlatformLiveID string
	Status         string
	StartedAt      *time.Time
	EndedAt        *time.Time
	TotalComments  int
	TotalOrders    int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

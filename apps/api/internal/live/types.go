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
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
}

type CreateLiveResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Platform  string    `json:"platform"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateLiveRequest struct {
	Title          string `json:"title" validate:"required,min=1,max=200"`
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
}

type LiveResponse struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Platform       string     `json:"platform"`
	PlatformLiveID string     `json:"platformLiveId"`
	Status         string     `json:"status"`
	StartedAt      *time.Time `json:"startedAt"`
	EndedAt        *time.Time `json:"endedAt"`
	TotalComments  int        `json:"totalComments"`
	TotalOrders    int        `json:"totalOrders"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type ListLivesResponse struct {
	Data       []LiveResponse           `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type LiveStatsResponse struct {
	TotalLives  int `json:"totalLives"`
	ActiveLives int `json:"activeLives"`
	TotalOrders int `json:"totalOrders"`
}

// EndLiveRequest represents the request body for ending a live session.
type EndLiveRequest struct {
	AutoSendCheckoutLinks *bool `json:"autoSendCheckoutLinks"` // Optional override
}

// EndLiveResponse represents the response after ending a live session.
type EndLiveResponse struct {
	Live           LiveResponse `json:"live"`
	CartsFinalized int          `json:"cartsFinalized"`
	AutoSendLinks  bool         `json:"autoSendLinks"`
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

// EndLiveInput represents input for ending a live session.
type EndLiveInput struct {
	ID       string
	StoreID  string
	AutoSend *bool // Override store's auto_send_checkout_links setting (nil = use store default)
}

// EndLiveOutput represents the result of ending a live session.
type EndLiveOutput struct {
	Live           LiveOutput
	CartsFinalized int  // Number of carts moved to checkout
	AutoSendLinks  bool // Whether checkout links will be sent automatically
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

type LiveStatsOutput struct {
	TotalLives  int
	ActiveLives int
	TotalOrders int
}

// =============================================================================
// CART - Service layer
// =============================================================================

// AddToCartInput represents input for adding a product to a user's cart during a live.
type AddToCartInput struct {
	SessionID      string
	PlatformUserID string
	PlatformHandle string
	ProductID      string
	ProductPrice   int64
	Quantity       int
}

// AddToCartOutput represents the result of adding to cart.
type AddToCartOutput struct {
	CartID    string
	IsNewCart bool
}

// =============================================================================
// CART - Repository layer
// =============================================================================

// GetOrCreateCartParams represents parameters for GetOrCreateCart.
type GetOrCreateCartParams struct {
	SessionID      string
	PlatformUserID string
	PlatformHandle string
	Token          string
}

// CartRow represents a cart row from the database.
type CartRow struct {
	ID             string
	SessionID      string
	PlatformUserID string
	PlatformHandle string
	Token          string
}

// AddCartItemParams represents parameters for adding an item to a cart.
type AddCartItemParams struct {
	CartID    string
	ProductID string
	Quantity  int
	UnitPrice int64
}

// =============================================================================
// LIVE SESSION - Repository layer
// =============================================================================

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

// =============================================================================
// PLATFORM AGGREGATION - Handler layer
// =============================================================================

// AddPlatformRequest represents the request to add a platform ID to a session.
type AddPlatformRequest struct {
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
}

// PlatformResponse represents a platform ID associated with a session.
type PlatformResponse struct {
	ID             string    `json:"id"`
	Platform       string    `json:"platform"`
	PlatformLiveID string    `json:"platformLiveId"`
	AddedAt        time.Time `json:"addedAt"`
}

// ListPlatformsResponse represents the list of platforms for a session.
type ListPlatformsResponse struct {
	Data []PlatformResponse `json:"data"`
}

// =============================================================================
// PLATFORM AGGREGATION - Service layer
// =============================================================================

// AddPlatformInput represents input for adding a platform to a session.
type AddPlatformInput struct {
	SessionID      string
	StoreID        string
	Platform       string
	PlatformLiveID string
}

// AddPlatformOutput represents the result of adding a platform.
type AddPlatformOutput struct {
	ID             string
	Platform       string
	PlatformLiveID string
	AddedAt        time.Time
}

// =============================================================================
// PLATFORM AGGREGATION - Repository layer
// =============================================================================

// PlatformRow represents a platform row from the database.
type PlatformRow struct {
	ID             string
	SessionID      string
	Platform       string
	PlatformLiveID string
	AddedAt        time.Time
}

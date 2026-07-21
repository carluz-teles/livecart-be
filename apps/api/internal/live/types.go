package live

import (
	"time"

	"livecart/apps/api/lib/query"
)

// =============================================================================
// LIVE EVENT - The container for sessions. Carts are tied to events.
// =============================================================================

// Handler layer - Request/Response types for Events
type CreateEventRequest struct {
	Title string `json:"title" validate:"required,min=1,max=200"`
	Type  string `json:"type" validate:"required,oneof=single multi"`
	// Scheduling
	ScheduledAt *string `json:"scheduledAt"` // ISO8601 timestamp
	Description *string `json:"description" validate:"omitempty,max=2000"`
}

type CreateEventResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type EventResponse struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	Type                   string `json:"type"`
	Status                 string `json:"status"`
	TotalOrders            int    `json:"totalOrders"`
	CloseCartOnEventEnd    bool   `json:"closeCartOnEventEnd"`
	CartExpirationMinutes  *int   `json:"cartExpirationMinutes"`
	CartMaxQuantityPerItem *int   `json:"cartMaxQuantityPerItem"`
	SendOnLiveEnd          *bool  `json:"sendOnLiveEnd"`
	PixDiscountPercent     int    `json:"pixDiscountPercent"`
	// Scheduling
	ScheduledAt *time.Time `json:"scheduledAt"`
	EndsAt      *time.Time `json:"endsAt"`
	Description *string    `json:"description"`
	// Counts
	ProductCount int               `json:"productCount"`
	UpsellCount  int               `json:"upsellCount"`
	Sessions     []SessionResponse `json:"sessions,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

type ListEventsResponse struct {
	Data       []EventResponse          `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// EndEventRequest represents the request body for ending an event.
type EndEventRequest struct {
	SendOnLiveEnd *bool `json:"sendOnLiveEnd"` // Optional override
}

// EndEventResponse represents the response after ending an event.
type EndEventResponse struct {
	Event          EventResponse `json:"event"`
	CartsFinalized int           `json:"cartsFinalized"`
	AutoSendLinks  bool          `json:"autoSendLinks"`
}

// Service layer - Event
type CreateEventInput struct {
	StoreID                string
	Title                  string
	Type                   string // single or multi
	CloseCartOnEventEnd    *bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     *int
	// Scheduling
	ScheduledAt *time.Time
	Description *string
}

type CreateEventOutput struct {
	ID        string
	Title     string
	Type      string
	Status    string
	CreatedAt time.Time
}

type EndEventInput struct {
	ID       string
	StoreID  string
	AutoSend *bool // Override store's auto_send_checkout_links setting (nil = use store default)
}

type EndEventOutput struct {
	Event          EventOutput
	CartsFinalized int  // Number of carts moved to checkout
	AutoSendLinks  bool // Whether checkout links will be sent automatically
}

type EventOutput struct {
	ID                     string
	StoreID                string
	Title                  string
	Type                   string
	Status                 string
	TotalOrders            int
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	CurrentActiveProductID *string
	ProcessingPaused       bool
	// Scheduling
	ScheduledAt *time.Time
	EndsAt      *time.Time
	Description *string
	// Counts
	ProductCount int
	UpsellCount  int
	Sessions     []SessionOutput
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ListEventsInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    EventFilters
}

type ListEventsOutput struct {
	Events     []EventOutput
	Total      int
	Pagination query.Pagination
}

type EventFilters struct {
	Status   []string `query:"status"` // scheduled, active, ended
	DateFrom *string  `query:"dateFrom"`
	DateTo   *string  `query:"dateTo"`
}

// Repository layer - Event
type CreateEventParams struct {
	StoreID                string
	Title                  string
	Type                   string
	Status                 string
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	// Scheduling
	ScheduledAt *time.Time
	Description *string
}

type EventRow struct {
	ID                     string
	StoreID                string
	Title                  string
	Type                   string
	Status                 string
	TotalOrders            int
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	CurrentActiveProductID *string
	ProcessingPaused       bool
	// Scheduling: ScheduledAt is the start; EndsAt is the optional end.
	ScheduledAt *time.Time
	EndsAt      *time.Time
	Description *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// =============================================================================
// LIVE SESSION - Platform-agnostic broadcast with start/end times
// =============================================================================

// Handler layer - Request/Response types for Sessions
type CreateSessionRequest struct {
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
}

// EventPulse is a tiny change-signal for near-real-time dashboard refresh. The
// client polls it cheaply and only refetches the heavy lists when a field moves:
// Orders/OrdersChangedAt → carts; Comments → comments feed.
type EventPulse struct {
	Orders          int       `json:"orders"`          // new-cart counter (event.total_orders)
	Comments        int       `json:"comments"`        // total comments across sessions
	OrdersChangedAt time.Time `json:"ordersChangedAt"` // latest cart change (catches edits/payments)
}

// CommentResponse represents a single comment in the API response.
type CommentResponse struct {
	Handle string `json:"handle"`
	Text   string `json:"text"`
}

type SessionResponse struct {
	ID            string             `json:"id"`
	EventID       string             `json:"eventId"`
	Status        string             `json:"status"`
	StartedAt     *time.Time         `json:"startedAt"`
	EndedAt       *time.Time         `json:"endedAt"`
	TotalComments int                `json:"totalComments"`
	TotalCarts    int                `json:"totalCarts"`
	PaidCarts     int                `json:"paidCarts"`
	TotalRevenue  int64              `json:"totalRevenue"`
	PaidRevenue   int64              `json:"paidRevenue"`
	Platforms     []PlatformResponse `json:"platforms,omitempty"`
	Comments      []CommentResponse  `json:"comments,omitempty"`
	CreatedAt     time.Time          `json:"createdAt"`
	UpdatedAt     time.Time          `json:"updatedAt"`
}

// Service layer - Session
type CreateSessionInput struct {
	EventID        string
	StoreID        string
	Platform       string
	PlatformLiveID string
}

type CreateSessionOutput struct {
	ID        string
	EventID   string
	Status    string
	Platform  PlatformOutput
	CreatedAt time.Time
}

// CommentOutput represents a comment in the service layer.
type CommentOutput struct {
	Handle string
	Text   string
}

type SessionOutput struct {
	ID            string
	EventID       string
	Status        string
	StartedAt     *time.Time
	EndedAt       *time.Time
	TotalComments int
	TotalCarts    int
	PaidCarts     int
	TotalRevenue  int64
	PaidRevenue   int64
	Platforms     []PlatformOutput
	Comments      []CommentOutput
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Repository layer - Session
type CreateSessionParams struct {
	EventID string
	Status  string
}

type SessionRow struct {
	ID            string
	EventID       string
	Status        string
	StartedAt     *time.Time
	EndedAt       *time.Time
	TotalComments int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// =============================================================================
// PLATFORM - Platform IDs associated with sessions
// =============================================================================

// PlatformResponse represents a platform ID associated with a session.
type PlatformResponse struct {
	ID             string    `json:"id"`
	Platform       string    `json:"platform"`
	PlatformLiveID string    `json:"platformLiveId"`
	AddedAt        time.Time `json:"addedAt"`
}

type ListPlatformsResponse struct {
	Data []PlatformResponse `json:"data"`
}

type AddPlatformRequest struct {
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
}

// Service layer - Platform
type AddPlatformInput struct {
	SessionID      string
	Platform       string
	PlatformLiveID string
}

type PlatformOutput struct {
	ID             string
	SessionID      string
	Platform       string
	PlatformLiveID string
	AddedAt        time.Time
}

// Repository layer - Platform
type PlatformRow struct {
	ID             string
	SessionID      string
	Platform       string
	PlatformLiveID string
	AddedAt        time.Time
}

// Repository layer - Comment
type CommentRow struct {
	ID                string
	SessionID         string
	PlatformCommentID string
	PlatformUserID    string
	PlatformHandle    string
	Text              string
	HasPurchaseIntent bool
	// Hidden mirrors the Instagram hide state (kept in sync by the moderation
	// actions) so the UI's hide button can toggle hide ↔ unhide.
	Hidden    bool
	CreatedAt time.Time
}

// CommentModerationResponse is a comment returned to the moderation UI, including
// the Instagram comment ID needed to reply / hide / delete via the Graph API.
type CommentModerationResponse struct {
	ID                string    `json:"id"`
	PlatformCommentID string    `json:"platformCommentId"`
	Handle            string    `json:"handle"`
	Text              string    `json:"text"`
	HasPurchaseIntent bool      `json:"hasPurchaseIntent"`
	Hidden            bool      `json:"hidden"`
	CreatedAt         time.Time `json:"createdAt"`
}

// ListCommentsResponse wraps the moderation comment list.
type ListCommentsResponse struct {
	Data []CommentModerationResponse `json:"data"`
}

// PostMediaInput carries the Instagram post metadata when creating a post event.
type PostMediaInput struct {
	MediaID      string
	Permalink    string
	ThumbnailURL string
	Caption      string
}

// PostMedia is the stored post metadata for an event.
type PostMedia struct {
	MediaID       string `json:"mediaId"`
	Permalink     string `json:"permalink"`
	ThumbnailURL  string `json:"thumbnailUrl"`
	Caption       string `json:"caption"`
	WebhookActive bool   `json:"webhookActive"`
}

// PostEventRef is a lightweight reference to a post event for capture loops.
type PostEventRef struct {
	EventID       string
	StoreID       string
	MediaID       string
	Status        string
	WebhookActive bool
}

// CreatePostRequest is the HTTP payload to create a post-commerce event.
// StartsAt/EndsAt are optional ISO8601 timestamps (with timezone).
type CreatePostRequest struct {
	Title                  string   `json:"title"`
	MediaID                string   `json:"mediaId" validate:"required"`
	MediaPermalink         string   `json:"mediaPermalink"`
	MediaThumbnailURL      string   `json:"mediaThumbnailUrl"`
	MediaCaption           string   `json:"mediaCaption"`
	ProductIDs             []string `json:"productIds" validate:"required,min=1"`
	StartsAt               *string  `json:"startsAt"`
	EndsAt                 *string  `json:"endsAt"`
	CartExpirationMinutes  *int     `json:"cartExpirationMinutes"`
	CartMaxQuantityPerItem *int     `json:"cartMaxQuantityPerItem"`
}

// CreatePostInput is the input to create a post-commerce event.
type CreatePostInput struct {
	StoreID string
	// Type is the event discriminator: "post" (default) for feed posts/Reels, or
	// "story" for Stories (24h window, purchase intent captured via DM replies).
	Type                   string
	Title                  string
	MediaID                string
	MediaPermalink         string
	MediaThumbnailURL      string
	MediaCaption           string
	ProductIDs             []string
	StartsAt               *time.Time
	EndsAt                 *time.Time
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
}

// =============================================================================
// LEGACY TYPES - For backwards compatibility with existing /lives endpoint
// =============================================================================

// LiveFilters for legacy compatibility
type LiveFilters struct {
	Status   []string `query:"status"`   // scheduled, live, ended, cancelled
	Platform []string `query:"platform"` // instagram, tiktok, youtube, facebook
	DateFrom *string  `query:"dateFrom"`
	DateTo   *string  `query:"dateTo"`
}

// CreateLiveRequest - Creates an event with a session and platform
type CreateLiveRequest struct {
	Title          string  `json:"title" validate:"required,min=1,max=200"`
	Type           string  `json:"type" validate:"omitempty,oneof=single multi"`
	Platform       *string `json:"platform" validate:"omitempty,oneof=instagram"`
	PlatformLiveID *string `json:"platformLiveId" validate:"omitempty"`
	// Scheduling
	ScheduledAt *string `json:"scheduledAt"` // ISO8601 timestamp
	Description *string `json:"description" validate:"omitempty,max=2000"`
	// Cart settings (override store defaults)
	CloseCartOnEventEnd    *bool `json:"closeCartOnEventEnd"`
	CartExpirationMinutes  *int  `json:"cartExpirationMinutes" validate:"omitempty,min=5,max=1440"`
	CartMaxQuantityPerItem *int  `json:"cartMaxQuantityPerItem" validate:"omitempty,min=1,max=100"`
	SendOnLiveEnd          *bool `json:"sendOnLiveEnd"`
	// PixDiscountPercent (0-100). 0 disables the feature.
	PixDiscountPercent *int `json:"pixDiscountPercent" validate:"omitempty,min=0,max=100"`
}

type CreateLiveResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Type      string    `json:"type"`
	Platform  string    `json:"platform,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateLiveRequest struct {
	Title string `json:"title" validate:"required,min=1,max=200"`
	// Optional fields. When omitted, the existing value is preserved.
	PixDiscountPercent *int `json:"pixDiscountPercent" validate:"omitempty,min=0,max=100"`
}

type LiveResponse struct {
	ID                     string     `json:"id"`
	Title                  string     `json:"title"`
	Type                   string     `json:"type"`
	Platform               string     `json:"platform"`       // Primary platform (from first session)
	PlatformLiveID         string     `json:"platformLiveId"` // Primary platform live ID
	Status                 string     `json:"status"`
	StartedAt              *time.Time `json:"startedAt"`
	EndedAt                *time.Time `json:"endedAt"`
	TotalComments          int        `json:"totalComments"`
	TotalOrders            int        `json:"totalOrders"`
	CloseCartOnEventEnd    bool       `json:"closeCartOnEventEnd"`
	CartExpirationMinutes  *int       `json:"cartExpirationMinutes"`
	CartMaxQuantityPerItem *int       `json:"cartMaxQuantityPerItem"`
	SendOnLiveEnd          *bool      `json:"sendOnLiveEnd"`
	PixDiscountPercent     int        `json:"pixDiscountPercent"`
	// Scheduling
	ScheduledAt *time.Time `json:"scheduledAt"`
	EndsAt      *time.Time `json:"endsAt"`
	Description *string    `json:"description"`
	// Counts
	ProductCount int       `json:"productCount"`
	UpsellCount  int       `json:"upsellCount"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type ListLivesResponse struct {
	Data       []LiveResponse           `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type LiveStatsResponse struct {
	TotalLives   int   `json:"totalLives"`
	ActiveLives  int   `json:"activeLives"`
	TotalOrders  int   `json:"totalOrders"`
	TotalRevenue int64 `json:"totalRevenue"`
}

type EndLiveRequest struct {
	SendOnLiveEnd *bool `json:"sendOnLiveEnd"`
}

type EndLiveResponse struct {
	Live           LiveResponse `json:"live"`
	CartsFinalized int          `json:"cartsFinalized"`
	AutoSendLinks  bool         `json:"autoSendLinks"`
}

// Service layer - Legacy
type CreateLiveInput struct {
	StoreID                string
	Title                  string
	Type                   string
	Platform               *string
	PlatformLiveID         *string
	CloseCartOnEventEnd    *bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     *int
	// Scheduling
	ScheduledAt *time.Time
	Description *string
}

type CreateLiveOutput struct {
	ID        string
	Title     string
	Type      string
	Platform  string
	Status    string
	CreatedAt time.Time
}

type UpdateLiveInput struct {
	ID                 string
	StoreID            string
	Title              string
	PixDiscountPercent *int
}

type EndLiveInput struct {
	ID       string
	StoreID  string
	AutoSend *bool
}

type EndLiveOutput struct {
	Live           LiveOutput
	CartsFinalized int
	AutoSendLinks  bool
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
	ID                     string
	StoreID                string
	Title                  string
	Type                   string
	Platform               string // Primary platform
	PlatformLiveID         string // Primary platform live ID
	Status                 string
	StartedAt              *time.Time
	EndedAt                *time.Time
	TotalComments          int
	TotalOrders            int
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	// Scheduling: ScheduledAt is the start, EndsAt the optional scheduled end.
	ScheduledAt *time.Time
	EndsAt      *time.Time
	Description *string
	// Counts
	ProductCount int
	UpsellCount  int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type LiveStatsOutput struct {
	TotalLives   int
	ActiveLives  int
	TotalOrders  int
	TotalRevenue int64
}

// =============================================================================
// CART - Service layer
// =============================================================================

// AddToCartInput represents input for adding a product to a user's cart during a live.
type AddToCartInput struct {
	StoreID            string // Required - used to upsert the customer row
	EventID            string // Changed from SessionID to EventID
	SessionID          string // Optional - session the item is attributed to (first-touch)
	PlatformUserID     string
	PlatformHandle     string
	ProductID          string
	ProductPrice       int64
	Quantity           int     // Total quantity to add
	WaitlistedQuantity int     // How many of the quantity are waitlisted (0 = all available)
	CustomerID         *string // Optional - links cart to a customer (if set, skips upsert)
}

// AddToCartOutput represents the result of adding to cart.
type AddToCartOutput struct {
	CartID     string
	CartToken  string
	IsNewCart  bool
	TotalItems int   // Total items in cart after add
	TotalCents int64 // Total value in cents after add
}

// GetOrCreateCartParams represents parameters for GetOrCreateCart.
type GetOrCreateCartParams struct {
	EventID        string
	SessionID      *string // Optional - tracks which session created the cart
	PlatformUserID string
	PlatformHandle string
	Token          string
	CustomerID     *string // Optional - links cart to a customer
}

// CartRow represents a cart row from the database.
type CartRow struct {
	ID             string
	EventID        string // Changed from SessionID to EventID
	PlatformUserID string
	PlatformHandle string
	Token          string
}

// AddCartItemParams represents parameters for adding an item to a cart.
type AddCartItemParams struct {
	CartID             string
	ProductID          string
	SessionID          string // Optional - session the item is attributed to (first-touch)
	Quantity           int
	UnitPrice          int64
	WaitlistedQuantity int // How many of the quantity are waitlisted
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing for event details page
// =============================================================================

// Handler layer - Event details
type EventStatsResponse struct {
	// Funnel metrics
	TotalComments int `json:"totalComments"`
	TotalCarts    int `json:"totalCarts"`
	OpenCarts     int `json:"openCarts"`
	CheckoutCarts int `json:"checkoutCarts"`
	PaidCarts     int `json:"paidCarts"`
	// Product metrics
	TotalProductsSold int `json:"totalProductsSold"`
	// Revenue metrics
	ProjectedRevenue int64 `json:"projectedRevenue"`
	ConfirmedRevenue int64 `json:"confirmedRevenue"`
}

type CartWithTotalResponse struct {
	ID              string     `json:"id"`
	Token           string     `json:"token"`
	SessionID       *string    `json:"sessionId"`
	PlatformUserID  string     `json:"platformUserId"`
	PlatformHandle  string     `json:"platformHandle"`
	Status          string     `json:"status"`
	PaymentStatus   *string    `json:"paymentStatus"`
	TotalValue      int64      `json:"totalValue"`
	TotalItems      int        `json:"totalItems"`
	AvailableItems  int        `json:"availableItems"`
	WaitlistedItems int        `json:"waitlistedItems"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       *time.Time `json:"expiresAt"`
}

type ListCartsResponse struct {
	Data []CartWithTotalResponse `json:"data"`
}

type EventProductSalesResponse struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	ImageURL      *string `json:"imageUrl"`
	Keyword       string  `json:"keyword"`
	TotalQuantity int     `json:"totalQuantity"`
	TotalRevenue  int64   `json:"totalRevenue"`
}

type ListEventProductSalesResponse struct {
	Data []EventProductSalesResponse `json:"data"`
}

// Service layer - Event details
type EventStatsOutput struct {
	// Funnel metrics
	TotalComments int
	TotalCarts    int
	OpenCarts     int
	CheckoutCarts int
	PaidCarts     int
	// Product metrics
	TotalProductsSold int
	// Revenue metrics
	ProjectedRevenue int64
	ConfirmedRevenue int64
}

type CartWithTotalOutput struct {
	ID              string
	Token           string
	SessionID       *string
	PlatformUserID  string
	PlatformHandle  string
	Status          string
	PaymentStatus   *string
	TotalValue      int64
	TotalItems      int
	AvailableItems  int
	WaitlistedItems int
	CreatedAt       time.Time
	ExpiresAt       *time.Time
}

type EventProductSalesOutput struct {
	ID            string
	Name          string
	ImageURL      *string
	Keyword       string
	TotalQuantity int
	TotalRevenue  int64
}

// Repository layer - Event details
type EventStatsRow struct {
	TotalComments     int
	TotalCarts        int
	OpenCarts         int
	CheckoutCarts     int
	PaidCarts         int
	TotalProductsSold int
	ProjectedRevenue  int64
	ConfirmedRevenue  int64
}

type CartWithTotalRow struct {
	ID              string
	EventID         string
	SessionID       *string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	PaymentStatus   *string
	TotalValue      int64
	TotalItems      int
	AvailableItems  int
	WaitlistedItems int
	CreatedAt       time.Time
	ExpiresAt       *time.Time
}

type EventProductRow struct {
	ID            string
	Name          string
	ImageURL      *string
	Keyword       string
	TotalQuantity int
	TotalRevenue  int64
}

// =============================================================================
// ACTIVE CHECKOUTS — live merchant view of carts in checkout phase
// =============================================================================

type ActiveCheckoutRow struct {
	ID                   string
	PlatformHandle       string
	Token                string
	Status               string
	PaymentStatus        string
	CreatedAt            time.Time
	ExpiresAt            *time.Time
	InitialSubtotalCents int64
	CurrentSubtotalCents int64
	MutationCount        int
	LastMutationAt       *time.Time
}

type ActiveCheckoutOutput = ActiveCheckoutRow

type ActiveCheckoutResponse struct {
	ID                   string     `json:"id"`
	PlatformHandle       string     `json:"platformHandle"`
	Token                string     `json:"token"`
	Status               string     `json:"status"`
	PaymentStatus        string     `json:"paymentStatus,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	InitialSubtotalCents int64      `json:"initialSubtotalCents"`
	CurrentSubtotalCents int64      `json:"currentSubtotalCents"`
	DeltaCents           int64      `json:"deltaCents"`
	MutationCount        int        `json:"mutationCount"`
	LastMutationAt       *time.Time `json:"lastMutationAt,omitempty"`
}

type ListActiveCheckoutsResponse struct {
	Data []ActiveCheckoutResponse `json:"data"`
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// Handler layer - Live Mode
type SetActiveProductRequest struct {
	ProductID *string `json:"productId"` // nil to clear
}

type SetProcessingPausedRequest struct {
	Paused bool `json:"paused"`
}

type LiveModeStateResponse struct {
	ProcessingPaused bool                   `json:"processingPaused"`
	ActiveProduct    *ActiveProductResponse `json:"activeProduct"`
}

type ActiveProductResponse struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Keyword  string  `json:"keyword"`
	Price    int64   `json:"price"`
	ImageURL *string `json:"imageUrl"`
}

// Service layer - Live Mode
type LiveModeStateOutput struct {
	ProcessingPaused bool
	ActiveProduct    *ActiveProductOutput
}

type ActiveProductOutput struct {
	ID       string
	Name     string
	Keyword  string
	Price    int64
	ImageURL *string
}

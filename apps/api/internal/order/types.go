package order

import (
	"time"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type OrderFilters struct {
	Status        []string `query:"status"`        // active, checkout, completed, expired
	PaymentStatus []string `query:"paymentStatus"` // pending, paid, failed, refunded
	LiveSessionID *string  `query:"liveSessionId"`
	DateFrom      *string  `query:"dateFrom"`
	DateTo        *string  `query:"dateTo"`

	// Tri-state filter on whether the order has any shipment row.
	// nil = ignore; *true = only orders with at least one shipment;
	// *false = only orders without any shipment. Combined with ShipmentStatus
	// the latter wins (ShipmentStatus implies HasShipment=true).
	HasShipment *bool `query:"hasShipment"`

	// Filter orders whose latest shipment status is in this set. Empty/nil = ignore.
	// Values follow the normalized ShipmentStatus enum (in_transit, delivered, ...).
	ShipmentStatus []string `query:"shipmentStatus"`
}

// Handler layer - Request/Response types
type UpdateOrderRequest struct {
	Status        *string `json:"status" validate:"omitempty,oneof=active checkout completed expired"`
	PaymentStatus *string `json:"paymentStatus" validate:"omitempty,oneof=pending paid failed refunded"`
}

// UpdateShippingAddressRequest is the admin's "edit address" payload. State
// is required; the 2-letter UF guard mirrors the public checkout flow.
type UpdateShippingAddressRequest struct {
	ZipCode      string `json:"zipCode" validate:"required"`
	Street       string `json:"street" validate:"required"`
	Number       string `json:"number" validate:"required"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood" validate:"required"`
	City         string `json:"city" validate:"required"`
	State        string `json:"state" validate:"required,len=2"`
}

// RegenerateCheckoutResponse returns the data the admin needs to share with
// the buyer (the cart token + the new expiration). The frontend builds the
// public URL from the token because the base URL lives in the FE config.
type RegenerateCheckoutResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
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

	// Shipping dimensions (joined from products). Zero when the product has no
	// dimensions filled in — admin UIs should treat them as "missing" not "0"
	// and block create-shipment until the merchant fills them in.
	WeightGrams   int    `json:"weightGrams"`
	HeightCm      int    `json:"heightCm"`
	WidthCm       int    `json:"widthCm"`
	LengthCm      int    `json:"lengthCm"`
	PackageFormat string `json:"packageFormat"`
}

type OrderResponse struct {
	ID             string              `json:"id"`
	// Per-store sequential order number, starts at 1000 in each store. UI shows
	// "#{shortId}" to merchants and customers — the UUID stays as the URL key.
	ShortID        int                 `json:"shortId"`
	LiveSessionID  string              `json:"liveSessionId"`
	LiveTitle      string              `json:"liveTitle"`
	LivePlatform   string              `json:"livePlatform"`
	CustomerHandle string              `json:"customerHandle"`
	CustomerID     string              `json:"customerId"`
	// Customer name/email captured at checkout. Empty until the buyer fills the
	// checkout form.
	CustomerName  string `json:"customerName"`
	CustomerEmail string `json:"customerEmail"`
	// Mirrors the live event's freeShipping flag, used by the list to render
	// a "frete grátis" indicator without loading the full event.
	FreeShipping  bool   `json:"freeShipping"`
	Status         string              `json:"status"`
	PaymentStatus  string              `json:"paymentStatus"`
	// Latest shipment status (normalized enum). Empty string when the order has
	// no shipment yet.
	ShipmentStatus string              `json:"shipmentStatus"`
	// True when the buyer picked a shipping service at checkout. Lets the
	// admin list distinguish "buyer never selected anything" from "selected,
	// but no shipment row created yet".
	HasShipping    bool                `json:"hasShipping"`
	Items          []OrderItemResponse `json:"items"`
	// Lightweight preview (name/image/qty) so the list can render an avatar
	// stack without the full Items array. Populated only on list endpoints.
	ItemsPreview   []OrderItemPreviewResponse `json:"itemsPreview"`
	TotalItems     int                 `json:"totalItems"`
	TotalAmount    int64               `json:"totalAmount"`
	PaidAt         *time.Time          `json:"paidAt"`
	CreatedAt      time.Time           `json:"createdAt"`
	ExpiresAt      *time.Time          `json:"expiresAt"`
	// True only for the buyer's earliest paid order in this store. Frontend
	// renders a "Primeira venda" badge from this flag.
	IsFirstPurchase bool `json:"isFirstPurchase"`
}

type OrderItemPreviewResponse struct {
	ProductName  string  `json:"productName"`
	ProductImage *string `json:"productImage"`
	Quantity     int     `json:"quantity"`
}

// OrderDetailResponse includes everything the admin order-detail page needs:
// customer captured at checkout, delivery address, freight selection, the
// shipment created at the carrier (when any) with its tracking timeline, and
// store shipping defaults so the UI can pre-fill the create-shipment form.
// The `OrderResponse` embedding keeps list-page fields identical.
type OrderDetailResponse struct {
	OrderResponse
	// Cart token; the public buyer link is `${frontend_origin}/cart/${token}`.
	// Detail-only — the list endpoint does not expose this to keep the surface
	// area narrow.
	Token           string                        `json:"token"`
	Comments        []OrderCommentResponse        `json:"comments"`
	Customer        *OrderCustomerResponse        `json:"customer,omitempty"`
	ShippingAddress *OrderShippingAddressResponse `json:"shippingAddress,omitempty"`
	Shipping        *OrderShippingSelectionResp   `json:"shipping,omitempty"`
	Shipment        *OrderShipmentResponse        `json:"shipment,omitempty"`
	Store           *OrderStoreResponse           `json:"store,omitempty"`
}

// OrderCustomerResponse mirrors the customer_* columns on carts captured at checkout.
type OrderCustomerResponse struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Document string `json:"document"`
	Phone    string `json:"phone"`
}

// OrderShippingAddressResponse is the destination address captured at checkout.
// All fields are strings so the admin UI never has to coerce nulls.
type OrderShippingAddressResponse struct {
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

// OrderShippingSelectionResp mirrors CartShippingSelection from the checkout
// DTOs, kept provider-agnostic (serviceId is opaque string + provider name).
type OrderShippingSelectionResp struct {
	Provider      string `json:"provider"`
	ServiceID     string `json:"serviceId"`
	ServiceName   string `json:"serviceName"`
	Carrier       string `json:"carrier"`
	CostCents     int64  `json:"costCents"`
	RealCostCents int64  `json:"realCostCents"`
	DeadlineDays  int    `json:"deadlineDays"`
	FreeShipping  bool   `json:"freeShipping"`
}

// OrderShipmentResponse is the freight order created at the carrier + its
// timeline of tracking events. Absent when no shipment has been created yet.
type OrderShipmentResponse struct {
	ID                  string                   `json:"id"`
	Provider            string                   `json:"provider"`
	ProviderOrderID     string                   `json:"providerOrderId"`
	ProviderOrderNumber string                   `json:"providerOrderNumber"`
	TrackingCode        string                   `json:"trackingCode"`
	PublicTrackingURL   string                   `json:"publicTrackingUrl"`
	InvoiceKey          string                   `json:"invoiceKey"`
	InvoiceKind         string                   `json:"invoiceKind"`
	LabelURL            string                   `json:"labelUrl"`
	Status              string                   `json:"status"`
	StatusRawCode       int                      `json:"statusRawCode"`
	StatusRawName       string                   `json:"statusRawName"`
	CreatedAt           time.Time                `json:"createdAt"`
	UpdatedAt           time.Time                `json:"updatedAt"`
	Events              []OrderShipmentEventResp `json:"events"`
}

// OrderShipmentEventResp is a single row from shipment_tracking_events.
type OrderShipmentEventResp struct {
	Status      string    `json:"status"`
	RawCode     int       `json:"rawCode"`
	RawName     string    `json:"rawName"`
	Observation string    `json:"observation"`
	EventAt     time.Time `json:"eventAt"`
	Source      string    `json:"source"`
}

// OrderStoreResponse exposes the origin data needed to pre-fill create-shipment.
type OrderStoreResponse struct {
	ID                string                      `json:"id"`
	Name              string                      `json:"name"`
	LogoURL           *string                     `json:"logoUrl"`
	Document          string                      `json:"document"` // CNPJ
	Email             string                      `json:"email"`
	Phone             string                      `json:"phone"`
	Address           OrderStoreAddressResponse   `json:"address"`
	ShippingDefaults  OrderStoreShippingDefaults  `json:"shippingDefaults"`
}

type OrderStoreAddressResponse struct {
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

type OrderStoreShippingDefaults struct {
	PackageWeightGrams int    `json:"packageWeightGrams"`
	PackageFormat      string `json:"packageFormat"`
}

type OrderCommentResponse struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
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

// =============================================================================
// UPSELL / DOWNSELL TYPES
// =============================================================================

// OrderUpsellOutput is the per-order summary used by the dashboard upsell
// card. DeltaCents > 0 means the buyer added value at checkout (upsell);
// DeltaCents < 0 means they removed value (downsell). HasSnapshot is false
// for legacy orders that predate the feature — frontend renders a neutral
// empty state in that case.
type OrderUpsellOutput struct {
	HasSnapshot          bool                   `json:"hasSnapshot"`
	SnapshotTakenAt      *time.Time             `json:"snapshotTakenAt,omitempty"`
	InitialSubtotalCents int64                  `json:"initialSubtotalCents"`
	FinalSubtotalCents   int64                  `json:"finalSubtotalCents"`
	DeltaCents           int64                  `json:"deltaCents"`
	MutationCount        int                    `json:"mutationCount"`
	InitialItems         []OrderUpsellItem      `json:"initialItems"`
	Mutations            []OrderUpsellMutation  `json:"mutations"`
}

type OrderUpsellItem struct {
	ProductID string  `json:"productId"`
	Name      string  `json:"name"`
	Keyword   string  `json:"keyword,omitempty"`
	ImageURL  *string `json:"imageUrl,omitempty"`
	Quantity  int     `json:"quantity"`
	UnitPrice int64   `json:"unitPrice"`
}

type OrderUpsellMutation struct {
	ID             string    `json:"id"`
	ProductID      string    `json:"productId"`
	ProductName    string    `json:"productName"`
	Keyword        string    `json:"keyword,omitempty"`
	ImageURL       *string   `json:"imageUrl,omitempty"`
	MutationType   string    `json:"mutationType"`
	QuantityBefore int       `json:"quantityBefore"`
	QuantityAfter  int       `json:"quantityAfter"`
	UnitPrice      int64     `json:"unitPrice"`
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"createdAt"`
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
	ID              string
	ShortID         int
	LiveSessionID   string
	LiveTitle       string
	LivePlatform    string
	CustomerHandle  string
	CustomerID      string
	CustomerName    string
	CustomerEmail   string
	FreeShipping    bool
	Status          string
	PaymentStatus   string
	ShipmentStatus  string
	HasShipping     bool
	Items           []OrderItemOutput
	ItemsPreview    []OrderItemPreviewOutput
	TotalItems      int
	TotalAmount     int64
	PaidAt          *time.Time
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	IsFirstPurchase bool
}

type OrderItemPreviewOutput struct {
	ProductName  string
	ProductImage *string
	Quantity     int
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

	WeightGrams   int
	HeightCm      int
	WidthCm       int
	LengthCm      int
	PackageFormat string
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
	ID              string
	ShortID         int
	EventID         string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	PaymentStatus   string
	PaidAt          *time.Time
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	CustomerName    string
	CustomerEmail   string
	LiveTitle       string
	FreeShipping    bool
	LivePlatform    string
	TotalAmount     int64
	TotalItems      int
	IsFirstPurchase bool
	// Latest shipment status for the cart, "" when no shipment exists yet.
	ShipmentStatus  string
	// True when the buyer picked a shipping service at checkout, even if no
	// shipment row has been created yet. Lets the list show "Aguardando emissão"
	// instead of "Sem envio" between checkout and shipment creation.
	HasShipping bool
}

// OrderItemPreviewRow is the projection used by the list page to render an
// avatar stack of products on each row without loading every cart_items column.
type OrderItemPreviewRow struct {
	ProductName  string
	ProductImage *string
	Quantity     int
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

	// Joined from products for the shipping flow. Zero when the product has
	// no dimensions filled in — service-layer sets them unchanged (0) so the
	// UI knows they are missing.
	WeightGrams   int
	HeightCm      int
	WidthCm       int
	LengthCm      int
	PackageFormat string
}

type OrderDetailRow struct {
	ID              string
	ShortID         int
	EventID         string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	PaymentStatus   string
	PaidAt          *time.Time
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	LiveTitle       string
	LivePlatform    string
	StoreID         string
	IsFirstPurchase bool

	// Customer captured at checkout (all optional — nil-safe reads).
	CustomerEmail    string
	CustomerName     string
	CustomerDocument string
	CustomerPhone    string

	// shipping_address JSONB — decoded into the fields below by the repo.
	ShippingAddressZip          string
	ShippingAddressStreet       string
	ShippingAddressNumber       string
	ShippingAddressComplement   string
	ShippingAddressNeighborhood string
	ShippingAddressCity         string
	ShippingAddressState        string

	// Cart freight selection (CartShippingSelection projection).
	ShippingProvider      string
	ShippingServiceID     string
	ShippingServiceName   string
	ShippingCarrier       string
	ShippingCostCents     int64
	ShippingCostRealCents int64
	ShippingDeadlineDays  int
	EventFreeShipping     bool

	// Store info for create-shipment pre-fill.
	StoreName                  string
	StoreLogoURL               *string
	StoreCNPJ                  string
	StoreEmail                 string
	StorePhone                 string
	StoreAddressZip            string
	StoreAddressStreet         string
	StoreAddressNumber         string
	StoreAddressComplement     string
	StoreAddressDistrict       string
	StoreAddressCity           string
	StoreAddressState          string
	StoreDefaultPkgWeightGrams int
	StoreDefaultPkgFormat      string
}

// OrderShipmentRecord is the projection of `shipments` used by order service.
// Kept local to avoid importing the integration package here (just a SQL shape).
type OrderShipmentRecord struct {
	ID                  string
	Provider            string
	ProviderOrderID     string
	ProviderOrderNumber string
	TrackingCode        string
	PublicTrackingURL   string
	InvoiceKey          string
	InvoiceKind         string
	LabelURL            string
	Status              string
	StatusRawCode       int
	StatusRawName       string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OrderShipmentEventRecord is the projection of `shipment_tracking_events`.
type OrderShipmentEventRecord struct {
	Status      string
	RawCode     int
	RawName     string
	Observation string
	EventAt     time.Time
	Source      string
}

type CommentRow struct {
	ID        string
	Text      string
	CreatedAt time.Time
}

type CommentOutput struct {
	ID        string
	Text      string
	CreatedAt time.Time
}

// OrderShippingAddressOutput is the parsed shipping_address JSONB projection.
type OrderShippingAddressOutput struct {
	ZipCode      string
	Street       string
	Number       string
	Complement   string
	Neighborhood string
	City         string
	State        string
}

// OrderCustomerOutput mirrors customer_* columns on carts.
type OrderCustomerOutput struct {
	Name     string
	Email    string
	Document string
	Phone    string
}

// OrderShippingOutput is the cart's chosen freight option.
type OrderShippingOutput struct {
	Provider      string
	ServiceID     string
	ServiceName   string
	Carrier       string
	CostCents     int64
	RealCostCents int64
	DeadlineDays  int
	FreeShipping  bool
}

// OrderStoreOutput is the store origin info.
type OrderStoreOutput struct {
	ID                string
	Name              string
	LogoURL           *string
	Document          string
	Email             string
	Phone             string
	Address           OrderShippingAddressOutput // reused shape
	PackageWeightGrams int
	PackageFormat     string
}

// OrderShipmentOutput bundles the shipment record + its tracking events.
type OrderShipmentOutput struct {
	OrderShipmentRecord
	Events []OrderShipmentEventRecord
}

type OrderDetailOutput struct {
	OrderOutput
	// Cart token used to build the public checkout link (/cart/{token}). Only
	// surfaced on the detail endpoint because the admin actions menu builds the
	// shareable URL from it.
	Token           string
	Comments        []CommentOutput
	Customer        *OrderCustomerOutput
	ShippingAddress *OrderShippingAddressOutput
	Shipping        *OrderShippingOutput
	Shipment        *OrderShipmentOutput
	Store           *OrderStoreOutput
}

package live

import "time"

// =============================================================================
// EVENT PRODUCTS - Handler Types
// =============================================================================

// EventProductRequest is the request to add/update a product in an event whitelist
type EventProductRequest struct {
	ProductID    string `json:"productId" validate:"required,uuid"`
	SpecialPrice *int64 `json:"specialPrice" validate:"omitempty,min=0"`
	MaxQuantity  *int32 `json:"maxQuantity" validate:"omitempty,min=1"`
	DisplayOrder int32  `json:"displayOrder"`
	Featured     bool   `json:"featured"`
}

// BulkEventProductsRequest is the request to set all products for an event
type BulkEventProductsRequest struct {
	Products []EventProductRequest `json:"products" validate:"required,dive"`
}

// EventProductResponse is the response for an event product
type EventProductResponse struct {
	ID             string  `json:"id"`
	ProductID      string  `json:"productId"`
	Name           string  `json:"name"`
	Keyword        string  `json:"keyword"`
	ImageURL       *string `json:"imageUrl"`
	OriginalPrice  int64   `json:"originalPrice"`
	SpecialPrice   *int64  `json:"specialPrice"`
	EffectivePrice int64   `json:"effectivePrice"`
	MaxQuantity    *int32  `json:"maxQuantity"`
	DisplayOrder   int32   `json:"displayOrder"`
	Featured       bool    `json:"featured"`
	Stock          int32   `json:"stock"`
	ProductActive  bool    `json:"productActive"`
}

// ListEventProductsResponse wraps the list of event products
type ListEventProductsResponse struct {
	Data []EventProductResponse `json:"data"`
}

// =============================================================================
// EVENT PRODUCTS - Service Types
// =============================================================================

// AddEventProductInput is the input for adding a product to an event
type AddEventProductInput struct {
	EventID      string
	StoreID      string
	ProductID    string
	SpecialPrice *int64
	MaxQuantity  *int32
	DisplayOrder int32
	Featured     bool
}

// UpdateEventProductInput is the input for updating an event product
type UpdateEventProductInput struct {
	ID           string
	EventID      string
	StoreID      string
	SpecialPrice *int64
	MaxQuantity  *int32
	DisplayOrder int32
	Featured     bool
}

// EventProductOutput is the output for an event product
type EventProductOutput struct {
	ID             string
	ProductID      string
	Name           string
	Keyword        string
	ImageURL       *string
	OriginalPrice  int64
	SpecialPrice   *int64
	EffectivePrice int64
	MaxQuantity    *int32
	DisplayOrder   int32
	Featured       bool
	Stock          int32
	ProductActive  bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// =============================================================================
// EVENT UPSELLS - Handler Types
// =============================================================================

// EventUpsellRequest is the request to add/update an upsell
type EventUpsellRequest struct {
	ProductID       string  `json:"productId" validate:"required,uuid"`
	DiscountPercent int32   `json:"discountPercent" validate:"min=0,max=100"`
	MessageTemplate *string `json:"messageTemplate" validate:"omitempty,max=500"`
	DisplayOrder    int32   `json:"displayOrder"`
	Active          bool    `json:"active"`
}

// EventUpsellResponse is the response for an event upsell
type EventUpsellResponse struct {
	ID              string  `json:"id"`
	ProductID       string  `json:"productId"`
	Name            string  `json:"name"`
	Keyword         string  `json:"keyword"`
	ImageURL        *string `json:"imageUrl"`
	OriginalPrice   int64   `json:"originalPrice"`
	DiscountPercent int32   `json:"discountPercent"`
	DiscountedPrice int64   `json:"discountedPrice"`
	MessageTemplate *string `json:"messageTemplate"`
	DisplayOrder    int32   `json:"displayOrder"`
	Active          bool    `json:"active"`
	Stock           int32   `json:"stock"`
}

// ListEventUpsellsResponse wraps the list of event upsells
type ListEventUpsellsResponse struct {
	Data []EventUpsellResponse `json:"data"`
}

// =============================================================================
// EVENT UPSELLS - Service Types
// =============================================================================

// AddEventUpsellInput is the input for adding an upsell to an event
type AddEventUpsellInput struct {
	EventID         string
	StoreID         string
	ProductID       string
	DiscountPercent int32
	MessageTemplate *string
	DisplayOrder    int32
	Active          bool
}

// UpdateEventUpsellInput is the input for updating an event upsell
type UpdateEventUpsellInput struct {
	ID              string
	EventID         string
	StoreID         string
	DiscountPercent int32
	MessageTemplate *string
	DisplayOrder    int32
	Active          bool
}

// EventUpsellOutput is the output for an event upsell
type EventUpsellOutput struct {
	ID              string
	ProductID       string
	Name            string
	Keyword         string
	ImageURL        *string
	OriginalPrice   int64
	DiscountPercent int32
	DiscountedPrice int64
	MessageTemplate *string
	DisplayOrder    int32
	Active          bool
	Stock           int32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// =============================================================================
// PRODUCT VALIDATION
// =============================================================================

// ProductValidationResult contains the result of validating a product for an event
type ProductValidationResult struct {
	ProductID      string
	ProductName    string
	Keyword        string
	OriginalPrice  int64
	EffectivePrice int64
	SpecialPrice   *int64
	MaxQuantity    *int32
	Stock          int32
	IsAllowed      bool
	IsActive       bool
}

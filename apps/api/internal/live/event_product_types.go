package live

import "time"

// =============================================================================
// PRODUTOS DE UMA TRANSMISSÃO — tipos compartilhados
//
// Os DTOs de entrada por EVENTO saíram junto com as rotas por evento
// (EventProductRequest, BulkEventProductsRequest — esta última nunca teve rota
// nem chamador —, AddEventProductInput e UpdateEventProductInput). A entrada
// viva é SessionProductRequest/UpdateSessionProductRequest, em
// session_product_types.go, já na convenção ozzo.
//
// O que sobra aqui é a SAÍDA, que os handlers por sessão continuam usando: é o
// mesmo dado, só ancorado na transmissão (SessionProductOutput é alias de
// EventProductOutput, não cópia).
// =============================================================================

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

package product

import (
	"time"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type ProductFilters struct {
	Status         []string `query:"status"`         // active, inactive
	ExternalSource []string `query:"externalSource"` // manual, bling, tiny, shopify
	PriceMin       *int64   `query:"priceMin"`       // min price in cents
	PriceMax       *int64   `query:"priceMax"`       // max price in cents
	StockMin       *int     `query:"stockMin"`       // min stock
	StockMax       *int     `query:"stockMax"`       // max stock
	HasLowStock    *bool    `query:"hasLowStock"`    // stock <= 5
}

// ListProductsRequest represents the query parameters for listing products
type ListProductsRequest struct {
	Search     string           `query:"search"`
	Pagination query.Pagination `query:"pagination"`
	Sorting    query.Sorting    `query:"sorting"`
	Filters    ProductFilters   `query:"filters"`
}

// ListProductsResponse represents the paginated response for listing products
type ListProductsResponse struct {
	Data       []ProductResponse        `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// Handler layer - Create/Update
type CreateProductRequest struct {
	Name           string `json:"name" validate:"required,min=1,max=200"`
	ExternalID     string `json:"externalId"`
	ExternalSource string `json:"externalSource" validate:"required,oneof=bling tiny shopify manual"`
	Keyword        string `json:"keyword"`
	Price          int64  `json:"price"` // price in cents
	ImageURL       string `json:"imageUrl"`
	Stock          int    `json:"stock" validate:"min=0"`
}

type CreateProductResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateProductRequest struct {
	Name     string `json:"name" validate:"required,min=1,max=200"`
	Price    int64  `json:"price"` // price in cents
	ImageURL string `json:"imageUrl"`
	Stock    int    `json:"stock" validate:"min=0"`
	Active   bool   `json:"active"`
}

type ProductResponse struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ExternalID     string    `json:"externalId"`
	ExternalSource string    `json:"externalSource"`
	Keyword        string    `json:"keyword"`
	Price          int64     `json:"price"` // price in cents
	ImageURL       string    `json:"imageUrl"`
	Stock          int       `json:"stock"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Service layer
type ListProductsInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    ProductFilters
}

type ListProductsOutput struct {
	Products   []ProductOutput
	Total      int
	Pagination query.Pagination
}

type CreateProductInput struct {
	StoreID        string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          int64 // price in cents
	ImageURL       string
	Stock          int
}

type CreateProductOutput struct {
	ID        string
	Name      string
	Keyword   string
	CreatedAt time.Time
}

type UpdateProductInput struct {
	StoreID  string
	ID       string
	Name     string
	Price    int64 // price in cents
	ImageURL string
	Stock    int
	Active   bool
}

type ProductOutput struct {
	ID             string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          int64 // price in cents
	ImageURL       string
	Stock          int
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Repository layer
type ListProductsParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    ProductFilters
}

type ListProductsResult struct {
	Products []ProductRow
	Total    int
}

type CreateProductParams struct {
	StoreID        string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          int64 // price in cents
	ImageURL       string
	Stock          int
}

type UpdateProductParams struct {
	ID       string
	StoreID  string
	Name     string
	Price    int64 // price in cents
	ImageURL string
	Stock    int
	Active   bool
}

type GetByKeywordParams struct {
	StoreID string
	Keyword string
}

type ProductRow struct {
	ID             string
	StoreID        string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          int64 // price in cents
	ImageURL       string
	Stock          int
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Stats types
type ProductStatsResponse struct {
	TotalProducts int   `json:"totalProducts"`
	ActiveCount   int   `json:"activeCount"`
	LowStockCount int   `json:"lowStockCount"` // stock <= 5
	StockValue    int64 `json:"stockValue"`    // sum of price * stock in cents
}

type ProductStatsOutput struct {
	TotalProducts int
	ActiveCount   int
	LowStockCount int
	StockValue    int64
}

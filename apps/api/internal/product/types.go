package product

import (
	"time"

	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/query"
	vo "livecart/apps/api/lib/valueobject"
)

// ============================================
// Handler layer - Request/Response types
// ============================================

// ProductFilters represents filter options for listing products.
type ProductFilters struct {
	Status         []string `query:"status"`         // active, inactive
	ExternalSource []string `query:"externalSource"` // manual, bling, tiny, shopify
	PriceMin       *int64   `query:"priceMin"`       // min price in cents
	PriceMax       *int64   `query:"priceMax"`       // max price in cents
	StockMin       *int     `query:"stockMin"`       // min stock
	StockMax       *int     `query:"stockMax"`       // max stock
	HasLowStock    *bool    `query:"hasLowStock"`    // stock <= 5
}

// ListProductsRequest represents the query parameters for listing products.
type ListProductsRequest struct {
	Search     string           `query:"search"`
	Pagination query.Pagination `query:"pagination"`
	Sorting    query.Sorting    `query:"sorting"`
	Filters    ProductFilters   `query:"filters"`
}

// ListProductsResponse represents the paginated response for listing products.
type ListProductsResponse struct {
	Data       []ProductResponse        `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// CreateProductRequest represents the request body for creating a product.
type CreateProductRequest struct {
	Name           string `json:"name" validate:"required,min=1,max=200"`
	ExternalID     string `json:"externalId"`
	ExternalSource string `json:"externalSource" validate:"required,oneof=bling tiny shopify manual"`
	Keyword        string `json:"keyword"`
	Price          int64  `json:"price"` // price in cents
	ImageURL       string `json:"imageUrl"`
	Stock          int    `json:"stock" validate:"min=0"`
}

// CreateProductResponse represents the response for product creation.
type CreateProductResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"createdAt"`
}

// UpdateProductRequest represents the request body for updating a product.
type UpdateProductRequest struct {
	Name     string `json:"name" validate:"required,min=1,max=200"`
	Price    int64  `json:"price"` // price in cents
	ImageURL string `json:"imageUrl"`
	Stock    int    `json:"stock" validate:"min=0"`
	Active   bool   `json:"active"`
}

// ProductResponse represents a product in API responses.
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

// ProductStatsResponse represents product statistics.
type ProductStatsResponse struct {
	TotalProducts int   `json:"totalProducts"`
	ActiveCount   int   `json:"activeCount"`
	LowStockCount int   `json:"lowStockCount"` // stock <= 5
	StockValue    int64 `json:"stockValue"`    // sum of price * stock in cents
}

// ============================================
// Service layer - Input/Output types
// ============================================

// ListProductsInput represents service input for listing products.
type ListProductsInput struct {
	StoreID    vo.StoreID
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    ProductFilters
}

// ListProductsOutput represents service output for listing products.
type ListProductsOutput struct {
	Products   []ProductOutput
	Total      int
	Pagination query.Pagination
}

// CreateProductInput represents service input for creating a product.
type CreateProductInput struct {
	StoreID        vo.StoreID
	Name           string
	ExternalID     string
	ExternalSource domain.ExternalSource
	Keyword        string
	Price          vo.Money
	ImageURL       string
	Stock          int
}

// CreateProductOutput represents service output for product creation.
type CreateProductOutput struct {
	ID        string
	Name      string
	Keyword   string
	CreatedAt time.Time
}

// UpdateProductInput represents service input for updating a product.
type UpdateProductInput struct {
	StoreID  vo.StoreID
	ID       vo.ProductID
	Name     string
	Price    vo.Money
	ImageURL string
	Stock    int
	Active   bool
}

// ProductOutput represents a product in service layer output.
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

// ProductStatsOutput represents product statistics in service layer.
type ProductStatsOutput struct {
	TotalProducts int
	ActiveCount   int
	LowStockCount int
	StockValue    int64
}

// SyncFromERPInput represents service input for syncing a product from an ERP.
type SyncFromERPInput struct {
	StoreID        vo.StoreID
	ExternalID     string
	ExternalSource domain.ExternalSource
	Name           string
	Price          vo.Money
	ImageURL       string
	Stock          int
	Active         bool
}

// ============================================
// Repository layer - Params types
// ============================================

// ListProductsParams represents repository parameters for listing products.
type ListProductsParams struct {
	StoreID    vo.StoreID
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    ProductFilters
}

// ListProductsResult represents repository result for listing products.
type ListProductsResult struct {
	Products []*domain.Product
	Total    int
}

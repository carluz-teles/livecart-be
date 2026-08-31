package catalog

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// ============================================
// Views (service/repository output)
// ============================================

// Catalog is the read model for a catalog.
type Catalog struct {
	ID           string
	StoreID      string
	Name         string
	ProductCount int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CatalogProductView is a product as shown inside a catalog.
type CatalogProductView struct {
	ID       string
	Name     string
	Code     string // product keyword — the "código da live"
	Price    int64  // cents
	ImageURL string
	Stock    int
	Active   bool
	Position int
}

// ============================================
// Requests
// ============================================

// CreateCatalogRequest creates a catalog, optionally seeding its products.
type CreateCatalogRequest struct {
	Name       string   `json:"name"`
	ProductIDs []string `json:"productIds"`
}

func (r CreateCatalogRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 120)),
		validation.Field(&r.ProductIDs, validation.Each(is.UUID)),
	)
}

// UpdateCatalogRequest renames a catalog.
type UpdateCatalogRequest struct {
	Name string `json:"name"`
}

func (r UpdateCatalogRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 120)),
	)
}

// SetProductsRequest replaces the full membership of a catalog (order = position).
type SetProductsRequest struct {
	ProductIDs []string `json:"productIds"`
}

func (r SetProductsRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ProductIDs, validation.Each(is.UUID)),
	)
}

// SetEventCatalogRequest associates (or clears) an event's catalog.
// CatalogID nil/empty clears the association.
type SetEventCatalogRequest struct {
	CatalogID *string `json:"catalogId"`
}

func (r SetEventCatalogRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.CatalogID, validation.When(r.CatalogID != nil && *r.CatalogID != "", is.UUID)),
	)
}

// ============================================
// Responses
// ============================================

type CatalogResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ProductCount int    `json:"productCount"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

func NewCatalogResponse(c Catalog) CatalogResponse {
	return CatalogResponse{
		ID:           c.ID,
		Name:         c.Name,
		ProductCount: c.ProductCount,
		CreatedAt:    c.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    c.UpdatedAt.Format(time.RFC3339),
	}
}

func NewListCatalogsResponse(items []Catalog) []CatalogResponse {
	out := make([]CatalogResponse, len(items))
	for i, c := range items {
		out[i] = NewCatalogResponse(c)
	}
	return out
}

// CatalogProductResponse is a product inside a catalog (admin or buyer view).
type CatalogProductResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Code     string `json:"code"`
	Price    int64  `json:"price"`
	ImageURL string `json:"imageUrl"`
	Stock    int    `json:"stock"`
	Position int    `json:"position"`
}

func newCatalogProductResponse(p CatalogProductView) CatalogProductResponse {
	return CatalogProductResponse{
		ID:       p.ID,
		Name:     p.Name,
		Code:     p.Code,
		Price:    p.Price,
		ImageURL: p.ImageURL,
		Stock:    p.Stock,
		Position: p.Position,
	}
}

func newCatalogProductResponses(items []CatalogProductView) []CatalogProductResponse {
	out := make([]CatalogProductResponse, len(items))
	for i, p := range items {
		out[i] = newCatalogProductResponse(p)
	}
	return out
}

// CatalogDetailResponse is a catalog with its products (admin GET by id).
type CatalogDetailResponse struct {
	ID        string                   `json:"id"`
	Name      string                   `json:"name"`
	CreatedAt string                   `json:"createdAt"`
	UpdatedAt string                   `json:"updatedAt"`
	Products  []CatalogProductResponse `json:"products"`
}

func NewCatalogDetailResponse(c Catalog, products []CatalogProductView) CatalogDetailResponse {
	return CatalogDetailResponse{
		ID:        c.ID,
		Name:      c.Name,
		CreatedAt: c.CreatedAt.Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Format(time.RFC3339),
		Products:  newCatalogProductResponses(products),
	}
}

// PublicCatalogResponse is the buyer-facing catalog payload.
type PublicCatalogResponse struct {
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Products []CatalogProductResponse `json:"products"`
}

func NewPublicCatalogResponse(c Catalog, products []CatalogProductView) PublicCatalogResponse {
	return PublicCatalogResponse{
		ID:       c.ID,
		Name:     c.Name,
		Products: newCatalogProductResponses(products),
	}
}

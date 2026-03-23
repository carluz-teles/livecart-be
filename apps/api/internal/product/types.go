package product

import "time"

// Handler layer
type CreateProductRequest struct {
	Name           string   `json:"name" validate:"required,min=1,max=200"`
	ExternalID     string   `json:"external_id"`
	ExternalSource string   `json:"external_source" validate:"required,oneof=bling tiny shopify manual"`
	Keyword        string   `json:"keyword"`
	Price          string   `json:"price"`
	ImageURL       string   `json:"image_url"`
	Sizes          []string `json:"sizes"`
	Stock          int      `json:"stock" validate:"min=0"`
}

type CreateProductResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"created_at"`
}

type UpdateProductRequest struct {
	Name     string   `json:"name" validate:"required,min=1,max=200"`
	Price    string   `json:"price"`
	ImageURL string   `json:"image_url"`
	Sizes    []string `json:"sizes"`
	Stock    int      `json:"stock" validate:"min=0"`
	Active   bool     `json:"active"`
}

type ProductResponse struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ExternalID     string    `json:"external_id"`
	ExternalSource string    `json:"external_source"`
	Keyword        string    `json:"keyword"`
	Price          string    `json:"price"`
	ImageURL       string    `json:"image_url"`
	Sizes          []string  `json:"sizes"`
	Stock          int       `json:"stock"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Service layer
type CreateProductInput struct {
	StoreID        string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          string
	ImageURL       string
	Sizes          []string
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
	Price    string
	ImageURL string
	Sizes    []string
	Stock    int
	Active   bool
}

type ProductOutput struct {
	ID             string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          string
	ImageURL       string
	Sizes          []string
	Stock          int
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Repository layer
type CreateProductParams struct {
	StoreID        string
	Name           string
	ExternalID     string
	ExternalSource string
	Keyword        string
	Price          string
	ImageURL       string
	Sizes          []string
	Stock          int
}

type UpdateProductParams struct {
	ID       string
	StoreID  string
	Name     string
	Price    string
	ImageURL string
	Sizes    []string
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
	Price          string
	ImageURL       string
	Sizes          []string
	Stock          int
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

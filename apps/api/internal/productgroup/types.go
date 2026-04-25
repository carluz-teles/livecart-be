package productgroup

import (
	"time"

	productpkg "livecart/apps/api/internal/product"
	productdomain "livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/query"
	vo "livecart/apps/api/lib/valueobject"
)

// ============================================
// HTTP DTOs
// ============================================

type OptionRequest struct {
	Name   string   `json:"name" validate:"required,min=1,max=50"`
	Values []string `json:"values" validate:"required,min=1,dive,required,max=80"`
}

type VariantRequest struct {
	OptionValues []string                      `json:"optionValues" validate:"required,min=1,dive,required"`
	Price        int64                         `json:"price" validate:"min=0"`
	Stock        int                           `json:"stock" validate:"min=0"`
	SKU          string                        `json:"sku" validate:"omitempty,max=100"`
	Keyword      string                        `json:"keyword" validate:"omitempty,len=4"`
	ImageURL     string                        `json:"imageUrl"`
	Images       []string                      `json:"images" validate:"omitempty,dive,required"`
	Shipping     productpkg.ShippingProfileDTO `json:"shipping"`
	// ExternalID is set by ERP-import flows to keep parity with Tiny/Bling/etc child product IDs.
	// API clients that create groups manually should leave this empty.
	ExternalID string `json:"-"`
}

type CreateGroupRequest struct {
	Name           string           `json:"name" validate:"required,min=1,max=200"`
	Description    string           `json:"description"`
	ExternalID     string           `json:"externalId"`
	ExternalSource string           `json:"externalSource" validate:"omitempty,oneof=manual bling tiny shopify"`
	Options        []OptionRequest  `json:"options" validate:"required,min=1,dive"`
	GroupImages    []string         `json:"groupImages" validate:"omitempty,dive,required"`
	Variants       []VariantRequest `json:"variants" validate:"required,min=1,dive"`
}

type UpdateGroupRequest struct {
	Name        string `json:"name" validate:"required,min=1,max=200"`
	Description string `json:"description"`
}

type AddImageRequest struct {
	URL      string `json:"url" validate:"required,url"`
	Position int    `json:"position" validate:"min=0"`
}

type OptionValueResponse struct {
	ID       string `json:"id"`
	Value    string `json:"value"`
	Position int    `json:"position"`
}

type OptionResponse struct {
	ID       string                `json:"id"`
	Name     string                `json:"name"`
	Position int                   `json:"position"`
	Values   []OptionValueResponse `json:"values"`
}

type ImageResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Position int    `json:"position"`
}

type VariantResponse struct {
	ID           string                       `json:"id"`
	Keyword      string                       `json:"keyword"`
	OptionValues []productpkg.OptionValueRef  `json:"optionValues"`
	Price        int64                        `json:"price"`
	Stock        int                          `json:"stock"`
	SKU          string                       `json:"sku"`
	ImageURL     string                       `json:"imageUrl"`
	Images       []ImageResponse              `json:"images"`
}

type GroupSummaryResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	ExternalID    string    `json:"externalId"`
	ExternalSource string   `json:"externalSource"`
	VariantsCount int       `json:"variantsCount"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type GroupDetailResponse struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	ExternalID     string            `json:"externalId"`
	ExternalSource string            `json:"externalSource"`
	Options        []OptionResponse  `json:"options"`
	GroupImages    []ImageResponse   `json:"groupImages"`
	Variants       []VariantResponse `json:"variants"`
	CreatedAt      time.Time         `json:"createdAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
}

type ListGroupsResponse struct {
	Data       []GroupSummaryResponse   `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type CreateGroupResponse struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Variants  []CreatedVariantSummary `json:"variants"`
	CreatedAt time.Time               `json:"createdAt"`
}

type CreatedVariantSummary struct {
	ID           string   `json:"id"`
	Keyword      string   `json:"keyword"`
	OptionValues []string `json:"optionValues"`
}

// ============================================
// Service-layer types
// ============================================

type CreateGroupInput struct {
	StoreID        vo.StoreID
	Name           string
	Description    string
	ExternalID     string
	ExternalSource productdomain.ExternalSource
	Options        []OptionRequest
	GroupImages    []string
	Variants       []VariantRequest
}

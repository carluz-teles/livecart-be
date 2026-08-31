package product

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"

	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
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
	Shippable      *bool    `query:"shippable"`      // has full shipping profile
}

// ShippingProfileDTO carries the physical attributes needed to quote freight.
// Dimensions and weight are all-or-nothing: provide all four or leave them null.
type ShippingProfileDTO struct {
	WeightGrams         *int   `json:"weightGrams"`
	HeightCm            *int   `json:"heightCm"`
	WidthCm             *int   `json:"widthCm"`
	LengthCm            *int   `json:"lengthCm"`
	SKU                 string `json:"sku"`
	Barcode             string `json:"barcode"`
	PackageFormat       string `json:"packageFormat"`
	InsuranceValueCents *int64 `json:"insuranceValueCents"`
}

// Validate is the syntactic gate (ozzo) for the shipping sub-document.
func (d ShippingProfileDTO) Validate() error {
	return validation.ValidateStruct(&d,
		validation.Field(&d.WeightGrams, validation.Min(1)),
		validation.Field(&d.HeightCm, validation.Min(1)),
		validation.Field(&d.WidthCm, validation.Min(1)),
		validation.Field(&d.LengthCm, validation.Min(1)),
		validation.Field(&d.SKU, validation.Length(0, 100)),
		validation.Field(&d.PackageFormat, validation.In("box", "roll", "letter")),
		validation.Field(&d.InsuranceValueCents, validation.Min(int64(0))),
	)
}

// CreateProductRequest represents the request body for creating a product.
// To create a variant of an existing group, pass `groupId`. For a simple product, omit it.
type CreateProductRequest struct {
	Name           string             `json:"name"`
	ExternalID     string             `json:"externalId"`
	ExternalSource string             `json:"externalSource"`
	Keyword        string             `json:"keyword"`
	Price          int64              `json:"price"` // price in cents
	ImageURL       string             `json:"imageUrl"`
	Stock          int                `json:"stock"`
	Shipping       ShippingProfileDTO `json:"shipping"`
	GroupID        string             `json:"groupId"`
	Images         []string           `json:"images"`
}

// Validate is the syntactic gate (ozzo) for product creation.
func (r CreateProductRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 200)),
		validation.Field(&r.ExternalSource, validation.Required, validation.In("bling", "tiny", "shopify", "manual")),
		validation.Field(&r.Stock, validation.Min(0)),
		validation.Field(&r.GroupID, is.UUID),
		validation.Field(&r.Shipping),
		validation.Field(&r.Images, validation.Each(validation.Required)),
	)
}

// ToInput builds the usecase input, constructing the value objects (semantic
// validation). Returns error so an invalid VO surfaces as 422 via the ErrorHandler.
func (r CreateProductRequest) ToInput(storeID string) (CreateProductInput, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return CreateProductInput{}, httpx.ErrUnprocessable("invalid store ID")
	}

	externalSource, err := domain.NewExternalSource(r.ExternalSource)
	if err != nil {
		return CreateProductInput{}, httpx.ErrUnprocessable("invalid external source")
	}

	price, err := vo.NewMoney(r.Price)
	if err != nil {
		return CreateProductInput{}, httpx.ErrUnprocessable("invalid price")
	}

	shipping, err := shippingDTOToDomain(r.Shipping)
	if err != nil {
		return CreateProductInput{}, httpx.ErrUnprocessable(err.Error())
	}

	var groupID *vo.ID
	if r.GroupID != "" {
		gid, err := vo.NewID(r.GroupID)
		if err != nil {
			return CreateProductInput{}, httpx.ErrUnprocessable("invalid groupId")
		}
		groupID = &gid
	}

	return CreateProductInput{
		StoreID:        sid,
		Name:           r.Name,
		ExternalID:     r.ExternalID,
		ExternalSource: externalSource,
		Keyword:        r.Keyword,
		Price:          price,
		ImageURL:       r.ImageURL,
		Stock:          r.Stock,
		Shipping:       shipping,
		GroupID:        groupID,
		Images:         r.Images,
	}, nil
}

// CreateProductResponse represents the response for product creation.
type CreateProductResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Keyword   string    `json:"keyword"`
	CreatedAt time.Time `json:"createdAt"`
}

// NewCreateProductResponse maps a freshly created Product entity to its response.
func NewCreateProductResponse(p *domain.Product) CreateProductResponse {
	return CreateProductResponse{
		ID:        p.ID().String(),
		Name:      p.Name(),
		Keyword:   p.Keyword().String(),
		CreatedAt: p.CreatedAt(),
	}
}

// AddProductImageRequest is the body for POST /products/:id/images.
type AddProductImageRequest struct {
	URL      string `json:"url"`
	Position int    `json:"position"`
}

// Validate is the syntactic gate (ozzo) for attaching an image.
func (r AddProductImageRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.URL, validation.Required, is.URL),
		validation.Field(&r.Position, validation.Min(0)),
	)
}

// AddProductImageResponse represents the response for a newly attached image.
type AddProductImageResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Position int    `json:"position"`
}

// UploadProductImageResponse carries the permanent public URL of an uploaded
// product image file, to be stored as the product/variant imageUrl.
type UploadProductImageResponse struct {
	URL string `json:"url"`
}

// UpdateProductRequest represents the request body for updating a product.
type UpdateProductRequest struct {
	Name     string             `json:"name"`
	Price    int64              `json:"price"` // price in cents
	ImageURL string             `json:"imageUrl"`
	Stock    int                `json:"stock"`
	Active   bool               `json:"active"`
	Shipping ShippingProfileDTO `json:"shipping"`
}

// Validate is the syntactic gate (ozzo) for product update.
func (r UpdateProductRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 200)),
		validation.Field(&r.Stock, validation.Min(0)),
		validation.Field(&r.Shipping),
	)
}

// ToInput builds the usecase input, constructing the value objects.
func (r UpdateProductRequest) ToInput(storeID, productID string) (UpdateProductInput, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return UpdateProductInput{}, httpx.ErrUnprocessable("invalid store ID")
	}

	id, err := vo.NewProductID(productID)
	if err != nil {
		return UpdateProductInput{}, httpx.ErrUnprocessable("invalid product ID")
	}

	price, err := vo.NewMoney(r.Price)
	if err != nil {
		return UpdateProductInput{}, httpx.ErrUnprocessable("invalid price")
	}

	shipping, err := shippingDTOToDomain(r.Shipping)
	if err != nil {
		return UpdateProductInput{}, httpx.ErrUnprocessable(err.Error())
	}

	return UpdateProductInput{
		StoreID:  sid,
		ID:       id,
		Name:     r.Name,
		Price:    price,
		ImageURL: r.ImageURL,
		Stock:    r.Stock,
		Active:   r.Active,
		Shipping: shipping,
	}, nil
}

// ProductResponse represents a product in API responses.
// `groupId`, `optionValues` and `images` are populated for variants;
// for simple products `groupId` is empty and `optionValues`/`images` are empty arrays.
type ProductResponse struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	ExternalID     string             `json:"externalId"`
	ExternalSource string             `json:"externalSource"`
	Keyword        string             `json:"keyword"`
	Price          int64              `json:"price"` // price in cents
	ImageURL       string             `json:"imageUrl"`
	Stock          int                `json:"stock"`
	Active         bool               `json:"active"`
	Shipping       ShippingProfileDTO `json:"shipping"`
	Shippable      bool               `json:"shippable"`
	GroupID        string             `json:"groupId"`
	// GroupName is the variant group's base name (e.g. "Camiseta Básica"), used
	// as a short title so the long per-variant name doesn't dominate the UI.
	GroupName    string           `json:"groupName"`
	OptionValues []OptionValueRef `json:"optionValues"`
	Images       []string         `json:"images"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

// NewProductResponse maps a ProductView (domain entity + read-side enrichment)
// to its API response. This is the controller's outbound mapper: presentation
// knows the domain; the domain never knows the response.
func NewProductResponse(v *ProductView) ProductResponse {
	p := v.Product
	groupID := ""
	if g := p.GroupID(); g != nil {
		groupID = g.String()
	}

	images := v.Images
	if images == nil {
		images = []string{}
	}
	options := v.OptionValues
	if options == nil {
		options = []OptionValueRef{}
	}

	return ProductResponse{
		ID:             p.ID().String(),
		Name:           p.Name(),
		ExternalID:     p.ExternalID(),
		ExternalSource: p.ExternalSource().String(),
		Keyword:        p.Keyword().String(),
		Price:          p.Price().Cents(),
		ImageURL:       p.ImageURL(),
		Stock:          p.Stock(),
		Active:         p.Active(),
		Shipping:       shippingDomainToDTO(p.Shipping()),
		Shippable:      p.IsShippable(),
		GroupID:        groupID,
		GroupName:      v.GroupName,
		OptionValues:   options,
		Images:         images,
		CreatedAt:      p.CreatedAt(),
		UpdatedAt:      p.UpdatedAt(),
	}
}

// ListProductsResponse represents the paginated response for listing products.
type ListProductsResponse struct {
	Data       []ProductResponse        `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// NewListProductsResponse maps a slice of ProductViews plus pagination to the
// list response.
func NewListProductsResponse(views []*ProductView, pagination query.Pagination, total int) ListProductsResponse {
	data := make([]ProductResponse, len(views))
	for i, v := range views {
		data[i] = NewProductResponse(v)
	}
	return ListProductsResponse{
		Data:       data,
		Pagination: query.NewPaginationResponse(pagination, total),
	}
}

// ProductStatsResponse represents product statistics.
type ProductStatsResponse struct {
	TotalProducts int   `json:"totalProducts"`
	ActiveCount   int   `json:"activeCount"`
	LowStockCount int   `json:"lowStockCount"` // stock <= 5
	StockValue    int64 `json:"stockValue"`    // sum of price * stock in cents
}

// NewProductStatsResponse maps aggregated stats to the response.
func NewProductStatsResponse(s ProductStats) ProductStatsResponse {
	return ProductStatsResponse{
		TotalProducts: s.TotalProducts,
		ActiveCount:   s.ActiveCount,
		LowStockCount: s.LowStockCount,
		StockValue:    s.StockValue,
	}
}

// ============================================
// Service layer - Input types + read views
// ============================================

// ProductView pairs a Product domain entity with its read-side enrichment
// (gallery images, denormalized option values, and the variant group's base
// name). The service returns this so the presentation layer can render a full
// ProductResponse without the domain entity having to carry projection data.
type ProductView struct {
	Product      *domain.Product
	Images       []string
	OptionValues []OptionValueRef
	GroupName    string
}

// ListProductsInput represents service input for listing products.
type ListProductsInput struct {
	StoreID    vo.StoreID
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    ProductFilters
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
	Shipping       domain.ShippingProfile
	GroupID        *vo.ID   // optional — pass when creating a variant of an existing product_group
	Images         []string // optional — gallery URLs to attach to the variant
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
	Shipping domain.ShippingProfile
}

// ProductStats represents aggregated product statistics.
type ProductStats struct {
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
	SkipStock      bool // When true, preserve local stock (e.g. on a DB-error fail-safe)
	// SKU e Barcode vêm do PRODUTO no ERP, não do perfil de frete dele.
	// Vazio significa "o ERP não informou" e o valor local é preservado — nunca
	// apagado, porque o lojista pode tê-lo preenchido à mão.
	SKU     string
	Barcode string
	// Shipping is optional. When non-nil and complete, the local shipping
	// profile is replaced — useful so re-imports/syncs pull the latest
	// dimensions from the ERP. When nil, the existing local profile is kept.
	Shipping *domain.ShippingProfile
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

// ============================================
// Shipping DTO <-> domain mappers
// ============================================

// shippingDTOToDomain converts the HTTP DTO into the domain shipping profile.
func shippingDTOToDomain(dto ShippingProfileDTO) (domain.ShippingProfile, error) {
	format, err := domain.NewPackageFormat(dto.PackageFormat)
	if err != nil {
		return domain.ShippingProfile{}, err
	}
	return domain.ShippingProfile{
		WeightGrams:         dto.WeightGrams,
		HeightCm:            dto.HeightCm,
		WidthCm:             dto.WidthCm,
		LengthCm:            dto.LengthCm,
		SKU:                 dto.SKU,
		Barcode:             dto.Barcode,
		PackageFormat:       format,
		InsuranceValueCents: dto.InsuranceValueCents,
	}, nil
}

func shippingDomainToDTO(s domain.ShippingProfile) ShippingProfileDTO {
	format := s.PackageFormat.String()
	if format == "" {
		format = string(domain.PackageFormatBox)
	}
	return ShippingProfileDTO{
		WeightGrams:         s.WeightGrams,
		HeightCm:            s.HeightCm,
		WidthCm:             s.WidthCm,
		LengthCm:            s.LengthCm,
		SKU:                 s.SKU,
		Barcode:             s.Barcode,
		PackageFormat:       format,
		InsuranceValueCents: s.InsuranceValueCents,
	}
}

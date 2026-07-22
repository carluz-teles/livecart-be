package productgroup

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"

	productpkg "livecart/apps/api/internal/product"
	productdomain "livecart/apps/api/internal/product/domain"
	"livecart/apps/api/internal/productgroup/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
	vo "livecart/apps/api/lib/valueobject"
)

// ============================================
// Handler layer - Request types (ozzo validation)
// ============================================

type OptionRequest struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

func (r OptionRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 50)),
		validation.Field(&r.Values,
			validation.Required, validation.Length(1, 0),
			validation.Each(validation.Required, validation.Length(1, 80)),
		),
	)
}

type VariantRequest struct {
	OptionValues []string                      `json:"optionValues"`
	Price        int64                         `json:"price"`
	Stock        int                           `json:"stock"`
	SKU          string                        `json:"sku"`
	Keyword      string                        `json:"keyword"`
	ImageURL     string                        `json:"imageUrl"`
	Images       []string                      `json:"images"`
	Shipping     productpkg.ShippingProfileDTO `json:"shipping"`
	// ExternalID is set by ERP-import flows to keep parity with Tiny/Bling/etc child product IDs.
	// API clients that create groups manually should leave this empty.
	ExternalID string `json:"-"`
}

func (r VariantRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.OptionValues,
			validation.Required, validation.Length(1, 0),
			validation.Each(validation.Required),
		),
		validation.Field(&r.Price, validation.Min(int64(0))),
		validation.Field(&r.Stock, validation.Min(0)),
		validation.Field(&r.SKU, validation.Length(0, 100)),
		validation.Field(&r.Keyword, validation.When(r.Keyword != "", validation.Length(4, 4))),
		validation.Field(&r.Images, validation.Each(validation.Required)),
	)
}

type CreateGroupRequest struct {
	Name           string           `json:"name"`
	Description    string           `json:"description"`
	ExternalID     string           `json:"externalId"`
	ExternalSource string           `json:"externalSource"`
	Options        []OptionRequest  `json:"options"`
	GroupImages    []string         `json:"groupImages"`
	Variants       []VariantRequest `json:"variants"`
}

// Validate is the syntactic gate (ozzo). Semantic checks (VO construction,
// combination uniqueness) happen in ToInput and the service.
func (r CreateGroupRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 200)),
		validation.Field(&r.ExternalSource,
			validation.When(r.ExternalSource != "",
				validation.In("manual", "bling", "tiny", "shopify"))),
		validation.Field(&r.Options, validation.Required, validation.Length(1, 0)),
		validation.Field(&r.GroupImages, validation.Each(validation.Required)),
		validation.Field(&r.Variants, validation.Required, validation.Length(1, 0)),
	)
}

// ToInput builds the usecase input, constructing the StoreID and ExternalSource
// value objects. Returns error so an invalid VO surfaces as 422 via the ErrorHandler.
func (r CreateGroupRequest) ToInput(storeID string) (CreateGroupInput, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return CreateGroupInput{}, httpx.ErrUnprocessable("invalid store ID")
	}
	// NewExternalSource defaults empty to "manual".
	src, err := productdomain.NewExternalSource(r.ExternalSource)
	if err != nil {
		return CreateGroupInput{}, httpx.ErrUnprocessable("invalid external source")
	}
	return CreateGroupInput{
		StoreID:        sid,
		Name:           r.Name,
		Description:    r.Description,
		ExternalID:     r.ExternalID,
		ExternalSource: src,
		Options:        r.Options,
		GroupImages:    r.GroupImages,
		Variants:       r.Variants,
	}, nil
}

type UpdateGroupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (r UpdateGroupRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(1, 200)),
	)
}

type AddImageRequest struct {
	URL      string `json:"url"`
	Position int    `json:"position"`
}

func (r AddImageRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.URL, validation.Required, is.URL),
		validation.Field(&r.Position, validation.Min(0)),
	)
}

// ============================================
// Handler layer - Response types + outbound mappers
// ============================================

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

// NewImageResponse maps a domain Image to its API response.
func NewImageResponse(img domain.Image) ImageResponse {
	return ImageResponse{ID: img.ID, URL: img.URL, Position: img.Position}
}

type VariantResponse struct {
	ID           string                      `json:"id"`
	Keyword      string                      `json:"keyword"`
	OptionValues []productpkg.OptionValueRef `json:"optionValues"`
	Price        int64                       `json:"price"`
	Stock        int                         `json:"stock"`
	SKU          string                      `json:"sku"`
	ImageURL     string                      `json:"imageUrl"`
	Images       []ImageResponse             `json:"images"`
}

type GroupSummaryResponse struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	ExternalID     string    `json:"externalId"`
	ExternalSource string    `json:"externalSource"`
	VariantsCount  int       `json:"variantsCount"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
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

// NewGroupDetailResponse maps the domain Detail aggregate to its API response.
// This is the controller's outbound mapper: presentation knows the domain; the
// domain never knows the response.
func NewGroupDetailResponse(d *domain.Detail) GroupDetailResponse {
	g := d.Group()

	optionsResp := make([]OptionResponse, len(d.Options()))
	for i, o := range d.Options() {
		valuesResp := make([]OptionValueResponse, len(o.Values))
		for j, v := range o.Values {
			valuesResp[j] = OptionValueResponse{ID: v.ID.String(), Value: v.Value, Position: v.Position}
		}
		optionsResp[i] = OptionResponse{ID: o.ID.String(), Name: o.Name, Position: o.Position, Values: valuesResp}
	}

	groupImages := make([]ImageResponse, len(d.GroupImages()))
	for i, img := range d.GroupImages() {
		groupImages[i] = NewImageResponse(img)
	}

	variants := make([]VariantResponse, len(d.Variants()))
	for i, v := range d.Variants() {
		ovs := make([]productpkg.OptionValueRef, len(v.OptionValues))
		for j, ov := range v.OptionValues {
			ovs[j] = productpkg.OptionValueRef{Option: ov.Option, Value: ov.Value}
		}
		imgs := make([]ImageResponse, len(v.Images))
		for j, img := range v.Images {
			imgs[j] = NewImageResponse(img)
		}
		variants[i] = VariantResponse{
			ID:           v.ID,
			Keyword:      v.Keyword,
			OptionValues: ovs,
			Price:        v.Price,
			Stock:        v.Stock,
			SKU:          v.SKU,
			ImageURL:     v.ImageURL,
			Images:       imgs,
		}
	}

	return GroupDetailResponse{
		ID:             g.ID().String(),
		Name:           g.Name(),
		Description:    g.Description(),
		ExternalID:     g.ExternalID(),
		ExternalSource: g.ExternalSource().String(),
		Options:        optionsResp,
		GroupImages:    groupImages,
		Variants:       variants,
		CreatedAt:      g.CreatedAt(),
		UpdatedAt:      g.UpdatedAt(),
	}
}

type ListGroupsResponse struct {
	Data       []GroupSummaryResponse   `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// NewListGroupsResponse maps a slice of Summary read-models to the list response.
func NewListGroupsResponse(items []domain.Summary, pag query.PaginationResponse) ListGroupsResponse {
	data := make([]GroupSummaryResponse, len(items))
	for i, s := range items {
		data[i] = GroupSummaryResponse{
			ID:             s.ID,
			Name:           s.Name,
			Description:    s.Description,
			ExternalID:     s.ExternalID,
			ExternalSource: s.ExternalSource,
			VariantsCount:  s.VariantsCount,
			CreatedAt:      s.CreatedAt,
			UpdatedAt:      s.UpdatedAt,
		}
	}
	return ListGroupsResponse{Data: data, Pagination: pag}
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

// NewCreateGroupResponse maps the domain CreateResult to its API response.
func NewCreateGroupResponse(r *domain.CreateResult) CreateGroupResponse {
	variants := make([]CreatedVariantSummary, len(r.Variants()))
	for i, v := range r.Variants() {
		variants[i] = CreatedVariantSummary{
			ID:           v.ID,
			Keyword:      v.Keyword,
			OptionValues: v.OptionValues,
		}
	}
	return CreateGroupResponse{
		ID:        r.ID(),
		Name:      r.Name(),
		Variants:  variants,
		CreatedAt: r.CreatedAt(),
	}
}

// ============================================
// Service layer - Input types (usecase returns domain aggregates)
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

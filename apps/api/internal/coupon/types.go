package coupon

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	"livecart/apps/api/internal/coupon/domain"
)

// Type and its constants are re-exported from the domain package so callers
// inside this package (service, repository) keep referring to coupon.Type /
// coupon.TypePercent without importing the domain package everywhere.
type Type = domain.Type

const (
	TypePercent      = domain.TypePercent
	TypeFixed        = domain.TypeFixed
	TypeFreeShipping = domain.TypeFreeShipping
)

// ============================================
// Handler layer - Response types
// ============================================

// CouponResponse is the admin-facing read shape returned by list/get/create/
// update. It is the controller's outbound mapper target: presentation knows
// the domain; the domain never knows the response.
type CouponResponse struct {
	ID               string     `json:"id"`
	EventID          string     `json:"eventId"`
	Code             string     `json:"code"`
	Type             Type       `json:"type"`
	ValueCents       int64      `json:"valueCents"`
	PercentBPS       int        `json:"percentBps"`
	MaxUses          *int       `json:"maxUses"`
	UsedCount        int        `json:"usedCount"`
	MinPurchaseCents int64      `json:"minPurchaseCents"`
	ValidFrom        *time.Time `json:"validFrom,omitempty"`
	ValidUntil       *time.Time `json:"validUntil,omitempty"`
	Active           bool       `json:"active"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

// NewCouponResponse maps a domain Coupon entity to its API response.
func NewCouponResponse(c *domain.Coupon) CouponResponse {
	return CouponResponse{
		ID:               c.ID(),
		EventID:          c.EventID(),
		Code:             c.Code(),
		Type:             c.Type(),
		ValueCents:       c.ValueCents(),
		PercentBPS:       c.PercentBPS(),
		MaxUses:          c.MaxUses(),
		UsedCount:        c.UsedCount(),
		MinPurchaseCents: c.MinPurchaseCents(),
		ValidFrom:        c.ValidFrom(),
		ValidUntil:       c.ValidUntil(),
		Active:           c.Active(),
		CreatedAt:        c.CreatedAt(),
		UpdatedAt:        c.UpdatedAt(),
	}
}

// ListCouponsResponse wraps the admin list.
type ListCouponsResponse struct {
	Data []CouponResponse `json:"data"`
}

// NewListCouponsResponse maps a slice of Coupon entities to the list response.
func NewListCouponsResponse(coupons []*domain.Coupon) ListCouponsResponse {
	data := make([]CouponResponse, len(coupons))
	for i, c := range coupons {
		data[i] = NewCouponResponse(c)
	}
	return ListCouponsResponse{Data: data}
}

// ApplyResponse is the buyer-side result returned after a successful public
// apply. The FE uses these to immediately reflect the new totals without a
// second round-trip. MaxDiscountCents is the merchant-set ceiling on a
// free-shipping coupon (0 = uncapped; ignored for percent / fixed).
type ApplyResponse struct {
	Code              string `json:"code"`
	Type              Type   `json:"type"`
	AppliedValueCents int64  `json:"appliedValueCents"`
	MaxDiscountCents  int64  `json:"maxDiscountCents"`
	SubtotalCents     int64  `json:"subtotalCents"`
	ShippingCostCents int64  `json:"shippingCostCents"`
	NewTotalCents     int64  `json:"newTotalCents"` // subtotal + shipping − applied
}

// NewApplyResponse maps the service-layer ApplyResult to the API response.
func NewApplyResponse(r *ApplyResult) ApplyResponse {
	return ApplyResponse{
		Code:              r.Code,
		Type:              r.Type,
		AppliedValueCents: r.AppliedValueCents,
		MaxDiscountCents:  r.MaxDiscountCents,
		SubtotalCents:     r.SubtotalCents,
		ShippingCostCents: r.ShippingCostCents,
		NewTotalCents:     r.NewTotalCents,
	}
}

// ============================================
// Handler layer - Request types (ozzo validation + ToInput)
// ============================================

// CreateRequest is the payload for POST .../coupons.
type CreateRequest struct {
	Code             string     `json:"code"`
	Type             Type       `json:"type"`
	ValueCents       int64      `json:"valueCents"`
	PercentBPS       int        `json:"percentBps"`
	MaxUses          *int       `json:"maxUses"`
	MinPurchaseCents int64      `json:"minPurchaseCents"`
	ValidFrom        *time.Time `json:"validFrom"`
	ValidUntil       *time.Time `json:"validUntil"`
	Active           bool       `json:"active"`
}

// Validate is the syntactic gate (ozzo). Business rules per type (percent
// requires percentBps > 0, etc.) live in the service.
func (r CreateRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Code, validation.Required, validation.Length(2, 40)),
		validation.Field(&r.Type, validation.Required,
			validation.In(TypePercent, TypeFixed, TypeFreeShipping)),
		validation.Field(&r.ValueCents, validation.Min(int64(0))),
		validation.Field(&r.PercentBPS, validation.Min(0), validation.Max(10000)),
		validation.Field(&r.MaxUses, validation.Min(1)),
		validation.Field(&r.MinPurchaseCents, validation.Min(int64(0))),
	)
}

// ToInput builds the create usecase input, binding the store/event context
// the handler pulled from the request.
func (r CreateRequest) ToInput(storeID, eventID string) (CreateInput, error) {
	return CreateInput{
		StoreID:          storeID,
		EventID:          eventID,
		Code:             r.Code,
		Type:             r.Type,
		ValueCents:       r.ValueCents,
		PercentBPS:       r.PercentBPS,
		MaxUses:          r.MaxUses,
		MinPurchaseCents: r.MinPurchaseCents,
		ValidFrom:        r.ValidFrom,
		ValidUntil:       r.ValidUntil,
		Active:           r.Active,
	}, nil
}

// UpdateRequest is the payload for PATCH .../coupons/:id. All fields are
// optional so the FE can do partial edits ("disable this one" without
// resending the whole row). Code is intentionally NOT updatable — it would
// orphan any in-flight redemptions and confuse buyers who copied the
// previous code.
type UpdateRequest struct {
	Type             *Type      `json:"type"`
	ValueCents       *int64     `json:"valueCents"`
	PercentBPS       *int       `json:"percentBps"`
	MaxUses          *int       `json:"maxUses"`
	MinPurchaseCents *int64     `json:"minPurchaseCents"`
	ValidFrom        *time.Time `json:"validFrom"`
	ValidUntil       *time.Time `json:"validUntil"`
	Active           *bool      `json:"active"`
}

// Validate is the syntactic gate (ozzo) for partial updates: every rule is
// only enforced when the field is present (nil pointers are skipped).
func (r UpdateRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Type,
			validation.In(TypePercent, TypeFixed, TypeFreeShipping)),
		validation.Field(&r.ValueCents, validation.Min(int64(0))),
		validation.Field(&r.PercentBPS, validation.Min(0), validation.Max(10000)),
		validation.Field(&r.MaxUses, validation.Min(1)),
		validation.Field(&r.MinPurchaseCents, validation.Min(int64(0))),
	)
}

// ToInput builds the update usecase input.
func (r UpdateRequest) ToInput(storeID, eventID, couponID string) (UpdateInput, error) {
	return UpdateInput{
		StoreID:          storeID,
		EventID:          eventID,
		CouponID:         couponID,
		Type:             r.Type,
		ValueCents:       r.ValueCents,
		PercentBPS:       r.PercentBPS,
		MaxUses:          r.MaxUses,
		MinPurchaseCents: r.MinPurchaseCents,
		ValidFrom:        r.ValidFrom,
		ValidUntil:       r.ValidUntil,
		Active:           r.Active,
	}, nil
}

// ApplyCouponRequest is the body for POST /api/public/checkout/:token/coupon.
type ApplyCouponRequest struct {
	Code string `json:"code"`
}

// Validate is the syntactic gate (ozzo) for the public apply.
func (r ApplyCouponRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Code, validation.Required, validation.Length(2, 40)),
	)
}

// ============================================
// Service layer - Input types (usecase returns *domain.Coupon)
// ============================================

type CreateInput struct {
	StoreID          string
	EventID          string
	Code             string
	Type             Type
	ValueCents       int64
	PercentBPS       int
	MaxUses          *int
	MinPurchaseCents int64
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	Active           bool
}

type UpdateInput struct {
	StoreID          string
	EventID          string
	CouponID         string
	Type             *Type
	ValueCents       *int64
	PercentBPS       *int
	MaxUses          *int
	MinPurchaseCents *int64
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	Active           *bool
}

// ApplyResult is the service-layer result of a successful public apply. It is
// mapped to ApplyResponse by the handler (NewApplyResponse). Kept as its own
// type because it is computed from the cart + coupon snapshot, not read
// straight off a domain.Coupon.
type ApplyResult struct {
	Code              string
	Type              Type
	AppliedValueCents int64
	MaxDiscountCents  int64
	SubtotalCents     int64
	ShippingCostCents int64
	NewTotalCents     int64 // subtotal + shipping − applied
}

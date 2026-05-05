package coupon

import "time"

// Type identifies how a coupon adjusts the cart total.
//
//   - percent       — applies PercentBPS (basis points; 1000 = 10%) on the
//     cart subtotal. Service layer caps the discount at the
//     subtotal so a 100% coupon on a R$50 cart never produces
//     a negative total.
//   - fixed         — subtracts ValueCents from the subtotal, capped at the
//     subtotal for the same reason as above.
//   - free_shipping — zeroes the cart's shipping line at apply time. Does
//     not touch ValueCents / PercentBPS.
type Type string

const (
	TypePercent      Type = "percent"
	TypeFixed        Type = "fixed"
	TypeFreeShipping Type = "free_shipping"
)

// Coupon is the admin-facing read shape returned by list/get.
type Coupon struct {
	ID                string     `json:"id"`
	EventID           string     `json:"eventId"`
	Code              string     `json:"code"`
	Type              Type       `json:"type"`
	ValueCents        int64      `json:"valueCents"`
	PercentBPS        int        `json:"percentBps"`
	MaxUses           *int       `json:"maxUses"`
	UsedCount         int        `json:"usedCount"`
	MinPurchaseCents  int64      `json:"minPurchaseCents"`
	ValidFrom         *time.Time `json:"validFrom,omitempty"`
	ValidUntil        *time.Time `json:"validUntil,omitempty"`
	Active            bool       `json:"active"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// CreateRequest is the payload for POST .../coupons.
type CreateRequest struct {
	Code             string     `json:"code" validate:"required,min=2,max=40"`
	Type             Type       `json:"type" validate:"required,oneof=percent fixed free_shipping"`
	ValueCents       int64      `json:"valueCents" validate:"gte=0"`
	PercentBPS       int        `json:"percentBps" validate:"gte=0,lte=10000"`
	MaxUses          *int       `json:"maxUses" validate:"omitempty,gte=1"`
	MinPurchaseCents int64      `json:"minPurchaseCents" validate:"gte=0"`
	ValidFrom        *time.Time `json:"validFrom"`
	ValidUntil       *time.Time `json:"validUntil"`
	Active           bool       `json:"active"`
}

// UpdateRequest is the payload for PATCH .../coupons/:id. All fields are
// optional so the FE can do partial edits ("disable this one" without
// resending the whole row). Code is intentionally NOT updatable — it would
// orphan any in-flight redemptions and confuse buyers who copied the
// previous code.
type UpdateRequest struct {
	Type             *Type      `json:"type" validate:"omitempty,oneof=percent fixed free_shipping"`
	ValueCents       *int64     `json:"valueCents" validate:"omitempty,gte=0"`
	PercentBPS       *int       `json:"percentBps" validate:"omitempty,gte=0,lte=10000"`
	MaxUses          *int       `json:"maxUses" validate:"omitempty,gte=1"`
	MinPurchaseCents *int64     `json:"minPurchaseCents" validate:"omitempty,gte=0"`
	ValidFrom        *time.Time `json:"validFrom"`
	ValidUntil       *time.Time `json:"validUntil"`
	Active           *bool      `json:"active"`
}

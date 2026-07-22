// Package domain holds the coupon aggregate: the entity the service and
// repository operate on. Fields are private and only reachable through
// getters so callers can't mutate a coupon out from under the invariants;
// Reconstruct rebuilds an instance from a persistence row (no validation —
// the DB is the source of truth for stored rows).
package domain

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

// Coupon is the coupon aggregate root.
type Coupon struct {
	id               string
	eventID          string
	code             string
	couponType       Type
	valueCents       int64
	percentBPS       int
	maxUses          *int
	usedCount        int
	minPurchaseCents int64
	validFrom        *time.Time
	validUntil       *time.Time
	active           bool
	createdAt        time.Time
	updatedAt        time.Time
}

// Reconstruct rebuilds a Coupon from persistence data. No validation: stored
// rows already passed the write-path invariants.
func Reconstruct(
	id string,
	eventID string,
	code string,
	couponType Type,
	valueCents int64,
	percentBPS int,
	maxUses *int,
	usedCount int,
	minPurchaseCents int64,
	validFrom *time.Time,
	validUntil *time.Time,
	active bool,
	createdAt time.Time,
	updatedAt time.Time,
) *Coupon {
	return &Coupon{
		id:               id,
		eventID:          eventID,
		code:             code,
		couponType:       couponType,
		valueCents:       valueCents,
		percentBPS:       percentBPS,
		maxUses:          maxUses,
		usedCount:        usedCount,
		minPurchaseCents: minPurchaseCents,
		validFrom:        validFrom,
		validUntil:       validUntil,
		active:           active,
		createdAt:        createdAt,
		updatedAt:        updatedAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (c *Coupon) ID() string              { return c.id }
func (c *Coupon) EventID() string         { return c.eventID }
func (c *Coupon) Code() string            { return c.code }
func (c *Coupon) Type() Type              { return c.couponType }
func (c *Coupon) ValueCents() int64       { return c.valueCents }
func (c *Coupon) PercentBPS() int         { return c.percentBPS }
func (c *Coupon) MaxUses() *int           { return c.maxUses }
func (c *Coupon) UsedCount() int          { return c.usedCount }
func (c *Coupon) MinPurchaseCents() int64 { return c.minPurchaseCents }
func (c *Coupon) ValidFrom() *time.Time   { return c.validFrom }
func (c *Coupon) ValidUntil() *time.Time  { return c.validUntil }
func (c *Coupon) Active() bool            { return c.active }
func (c *Coupon) CreatedAt() time.Time    { return c.createdAt }
func (c *Coupon) UpdatedAt() time.Time    { return c.updatedAt }

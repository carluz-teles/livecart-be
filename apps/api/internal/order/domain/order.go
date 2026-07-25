// Package domain defines the Order aggregate: a materialised, immutable snapshot
// of a purchase sealed at cart.paid. The snapshot fields (total_cents, items, etc.)
// are set-once in OnCartPaid and never mutated; only the fulfillment state
// (status, payment_status, shipment_status) evolves after that.
package domain

import (
	"errors"
	"time"
)

var (
	ErrOrderAlreadyPaid   = errors.New("order is already paid")
	ErrOrderNotModifiable = errors.New("order cannot be modified in current status")
	ErrOrderNotRefundable = errors.New("order payment cannot be refunded")
)

// Order is the aggregate root for the materialised purchase stored in `orders`.
type Order struct {
	id             string
	cartID         string
	shortID        int32
	storeID        string
	eventID        string
	customerID     *string
	status         OrderStatus
	totalCents     *int64
	discountCents  int64
	shippingCents  int64
	paidTotalCents *int64
	paidAt         *time.Time
	createdAt      time.Time
	updatedAt      time.Time
	items          []*OrderItem
	payment        *Payment
	logistics      *Logistics
}

func Reconstruct(
	id, cartID string,
	shortID int32,
	storeID, eventID string,
	customerID *string,
	status OrderStatus,
	totalCents *int64,
	discountCents, shippingCents int64,
	paidTotalCents *int64,
	paidAt *time.Time,
	createdAt, updatedAt time.Time,
) *Order {
	return &Order{
		id:             id,
		cartID:         cartID,
		shortID:        shortID,
		storeID:        storeID,
		eventID:        eventID,
		customerID:     customerID,
		status:         status,
		totalCents:     totalCents,
		discountCents:  discountCents,
		shippingCents:  shippingCents,
		paidTotalCents: paidTotalCents,
		paidAt:         paidAt,
		createdAt:      createdAt,
		updatedAt:      updatedAt,
	}
}

func (o *Order) ID() string          { return o.id }
func (o *Order) CartID() string      { return o.cartID }
func (o *Order) ShortID() int32      { return o.shortID }
func (o *Order) StoreID() string     { return o.storeID }
func (o *Order) EventID() string     { return o.eventID }
func (o *Order) CustomerID() *string { return o.customerID }
func (o *Order) Status() OrderStatus { return o.status }
func (o *Order) TotalCents() *int64  { return o.totalCents }
func (o *Order) DiscountCents() int64  { return o.discountCents }
func (o *Order) ShippingCents() int64  { return o.shippingCents }
func (o *Order) PaidTotalCents() *int64 { return o.paidTotalCents }
func (o *Order) PaidAt() *time.Time  { return o.paidAt }
func (o *Order) CreatedAt() time.Time { return o.createdAt }
func (o *Order) Items() []*OrderItem  { return o.items }
func (o *Order) Payment() *Payment    { return o.payment }
func (o *Order) Logistics() *Logistics { return o.logistics }

func (o *Order) SetItems(items []*OrderItem)      { o.items = items }
func (o *Order) SetPayment(p *Payment)            { o.payment = p }
func (o *Order) SetLogistics(l *Logistics)        { o.logistics = l }

func (o *Order) IsPaid() bool { return o.status.IsPaid() }

// ValidatePaidInvariant checks that a paid order has all required snapshot fields.
func (o *Order) ValidatePaidInvariant() error {
	if !o.IsPaid() {
		return nil
	}
	if o.totalCents == nil || o.paidTotalCents == nil || o.paidAt == nil {
		return errors.New("paid order must have total_cents, paid_total_cents and paid_at")
	}
	return nil
}

// ItemsTotal returns SUM(quantity * unit_price) across order_items.
// Must equal total_cents by design invariant.
func (o *Order) ItemsTotal() int64 {
	var total int64
	for _, item := range o.items {
		total += int64(item.Quantity()) * item.UnitPrice()
	}
	return total
}

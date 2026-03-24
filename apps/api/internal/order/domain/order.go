package domain

import (
	"errors"
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

// Domain errors
var (
	ErrOrderNotModifiable = errors.New("order cannot be modified in current status")
	ErrOrderNotRefundable = errors.New("order payment cannot be refunded")
	ErrInvalidQuantity    = errors.New("quantity must be positive")
)

// OrderItem represents an item in an order.
type OrderItem struct {
	id           string
	productID    vo.ProductID
	productName  string
	productImage *string
	keyword      string
	size         *string
	quantity     int
	unitPrice    vo.Money
}

// NewOrderItem creates a new OrderItem.
func NewOrderItem(
	id string,
	productID vo.ProductID,
	productName string,
	productImage *string,
	keyword string,
	size *string,
	quantity int,
	unitPrice vo.Money,
) (*OrderItem, error) {
	if quantity <= 0 {
		return nil, ErrInvalidQuantity
	}

	return &OrderItem{
		id:           id,
		productID:    productID,
		productName:  productName,
		productImage: productImage,
		keyword:      keyword,
		size:         size,
		quantity:     quantity,
		unitPrice:    unitPrice,
	}, nil
}

// Getters
func (i *OrderItem) ID() string             { return i.id }
func (i *OrderItem) ProductID() vo.ProductID { return i.productID }
func (i *OrderItem) ProductName() string    { return i.productName }
func (i *OrderItem) ProductImage() *string  { return i.productImage }
func (i *OrderItem) Keyword() string        { return i.keyword }
func (i *OrderItem) Size() *string          { return i.size }
func (i *OrderItem) Quantity() int          { return i.quantity }
func (i *OrderItem) UnitPrice() vo.Money    { return i.unitPrice }

// TotalPrice returns the total price for this item (quantity * unitPrice).
func (i *OrderItem) TotalPrice() vo.Money {
	return i.unitPrice.Multiply(i.quantity)
}

// Order represents a customer order.
type Order struct {
	id             vo.OrderID
	storeID        vo.StoreID
	sessionID      vo.LiveID
	customerID     vo.CustomerID
	customerHandle string
	liveTitle      string
	livePlatform   string
	status         OrderStatus
	paymentStatus  PaymentStatus
	items          []*OrderItem
	paidAt         *time.Time
	expiresAt      *time.Time
	createdAt      time.Time
}

// Reconstruct rebuilds an Order from persistence data.
func Reconstruct(
	id vo.OrderID,
	storeID vo.StoreID,
	sessionID vo.LiveID,
	customerID vo.CustomerID,
	customerHandle string,
	liveTitle string,
	livePlatform string,
	status OrderStatus,
	paymentStatus PaymentStatus,
	items []*OrderItem,
	paidAt *time.Time,
	expiresAt *time.Time,
	createdAt time.Time,
) *Order {
	return &Order{
		id:             id,
		storeID:        storeID,
		sessionID:      sessionID,
		customerID:     customerID,
		customerHandle: customerHandle,
		liveTitle:      liveTitle,
		livePlatform:   livePlatform,
		status:         status,
		paymentStatus:  paymentStatus,
		items:          items,
		paidAt:         paidAt,
		expiresAt:      expiresAt,
		createdAt:      createdAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (o *Order) ID() vo.OrderID            { return o.id }
func (o *Order) StoreID() vo.StoreID       { return o.storeID }
func (o *Order) SessionID() vo.LiveID      { return o.sessionID }
func (o *Order) CustomerID() vo.CustomerID { return o.customerID }
func (o *Order) CustomerHandle() string    { return o.customerHandle }
func (o *Order) LiveTitle() string         { return o.liveTitle }
func (o *Order) LivePlatform() string      { return o.livePlatform }
func (o *Order) Status() OrderStatus       { return o.status }
func (o *Order) PaymentStatus() PaymentStatus { return o.paymentStatus }
func (o *Order) Items() []*OrderItem       { return o.items }
func (o *Order) PaidAt() *time.Time        { return o.paidAt }
func (o *Order) ExpiresAt() *time.Time     { return o.expiresAt }
func (o *Order) CreatedAt() time.Time      { return o.createdAt }

// ============================================
// Business Rules
// ============================================

// TotalItems returns the total quantity of items in the order.
func (o *Order) TotalItems() int {
	total := 0
	for _, item := range o.items {
		total += item.Quantity()
	}
	return total
}

// TotalAmount returns the total amount of the order.
func (o *Order) TotalAmount() vo.Money {
	total := vo.Zero()
	for _, item := range o.items {
		total = total.Add(item.TotalPrice())
	}
	return total
}

// CanBeModified returns true if the order can be modified.
func (o *Order) CanBeModified() bool {
	return o.status.CanBeModified()
}

// CanBeRefunded returns true if the order payment can be refunded.
func (o *Order) CanBeRefunded() bool {
	return o.paymentStatus.CanBeRefunded()
}

// IsExpired returns true if the order has expired.
func (o *Order) IsExpired() bool {
	if o.expiresAt == nil {
		return false
	}
	return time.Now().After(*o.expiresAt)
}

// IsPaid returns true if the order has been paid.
func (o *Order) IsPaid() bool {
	return o.paymentStatus.IsPaid()
}

// BelongsToStore checks if the order belongs to the given store.
func (o *Order) BelongsToStore(storeID vo.StoreID) bool {
	return o.storeID.Equals(storeID)
}

// ============================================
// State Changes
// ============================================

// UpdateStatus updates the order status.
func (o *Order) UpdateStatus(status OrderStatus) error {
	if !o.CanBeModified() && !o.status.IsCompleted() {
		return ErrOrderNotModifiable
	}
	o.status = status
	return nil
}

// UpdatePaymentStatus updates the payment status.
func (o *Order) UpdatePaymentStatus(status PaymentStatus) error {
	// If marking as paid, set paidAt timestamp
	if status.IsPaid() && !o.paymentStatus.IsPaid() {
		now := time.Now()
		o.paidAt = &now
	}
	o.paymentStatus = status
	return nil
}

// MarkAsPaid marks the order as paid.
func (o *Order) MarkAsPaid() {
	now := time.Now()
	o.paymentStatus = PaymentPaid
	o.paidAt = &now
}

// MarkAsCompleted marks the order as completed.
func (o *Order) MarkAsCompleted() {
	o.status = StatusCompleted
}

// MarkAsExpired marks the order as expired.
func (o *Order) MarkAsExpired() {
	o.status = StatusExpired
}

// Refund marks the order payment as refunded.
func (o *Order) Refund() error {
	if !o.CanBeRefunded() {
		return ErrOrderNotRefundable
	}
	o.paymentStatus = PaymentRefunded
	return nil
}

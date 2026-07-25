package domain

import "errors"

var ErrInvalidQuantity = errors.New("quantity must be positive")

// OrderItem is an immutable line of the order — snapshot of a cart_item at payment.
type OrderItem struct {
	id          string
	orderID     string
	productID   string
	productName string
	quantity    int32
	unitPrice   int64
}

func NewOrderItem(id, orderID, productID, productName string, quantity int32, unitPrice int64) (*OrderItem, error) {
	if quantity <= 0 {
		return nil, ErrInvalidQuantity
	}
	return &OrderItem{
		id:          id,
		orderID:     orderID,
		productID:   productID,
		productName: productName,
		quantity:    quantity,
		unitPrice:   unitPrice,
	}, nil
}

func (i *OrderItem) ID() string          { return i.id }
func (i *OrderItem) OrderID() string     { return i.orderID }
func (i *OrderItem) ProductID() string   { return i.productID }
func (i *OrderItem) ProductName() string { return i.productName }
func (i *OrderItem) Quantity() int32     { return i.quantity }
func (i *OrderItem) UnitPrice() int64    { return i.unitPrice }

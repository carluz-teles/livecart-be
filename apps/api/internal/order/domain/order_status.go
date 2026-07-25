package domain

import (
	"database/sql/driver"
	"errors"
)

// OrderStatus é o ciclo coarse da Order (raiz). Detalhe fino mora em
// PaymentStatus (order_payments) e ShipmentStatus (order_logistics).
type OrderStatus struct{ value string }

var (
	StatusPendingPayment = OrderStatus{value: "pending_payment"} // draft (Fatia 2+)
	StatusPaid           = OrderStatus{value: "paid"}
	StatusShipped        = OrderStatus{value: "shipped"}
	StatusDelivered      = OrderStatus{value: "delivered"}
	StatusRefunded       = OrderStatus{value: "refunded"}
	StatusCancelled      = OrderStatus{value: "cancelled"}
	StatusExpired        = OrderStatus{value: "expired"}
)

var ErrInvalidOrderStatus = errors.New("invalid order status")

var validOrderStatuses = map[string]OrderStatus{
	"pending_payment": StatusPendingPayment,
	"paid":            StatusPaid,
	"shipped":         StatusShipped,
	"delivered":       StatusDelivered,
	"refunded":        StatusRefunded,
	"cancelled":       StatusCancelled,
	"expired":         StatusExpired,
}

func NewOrderStatus(raw string) (OrderStatus, error) {
	if raw == "" {
		return StatusPendingPayment, nil
	}
	s, ok := validOrderStatuses[raw]
	if !ok {
		return OrderStatus{}, ErrInvalidOrderStatus
	}
	return s, nil
}

func (s OrderStatus) String() string { return s.value }
func (s OrderStatus) IsZero() bool   { return s.value == "" }
func (s OrderStatus) IsPaid() bool   { return s.value == StatusPaid.value }

func (s OrderStatus) Value() (driver.Value, error) {
	if s.IsZero() {
		return "pending_payment", nil
	}
	return s.value, nil
}

func (s *OrderStatus) Scan(src any) error {
	switch v := src.(type) {
	case string:
		st, err := NewOrderStatus(v)
		if err != nil {
			s.value = v
			return nil
		}
		*s = st
	case []byte:
		st, err := NewOrderStatus(string(v))
		if err != nil {
			s.value = string(v)
			return nil
		}
		*s = st
	default:
		return ErrInvalidOrderStatus
	}
	return nil
}

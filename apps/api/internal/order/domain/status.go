package domain

import (
	"database/sql/driver"
	"errors"
)

// OrderStatus represents the status of an order.
type OrderStatus struct {
	value string
}

// Status constants
var (
	StatusPending   = OrderStatus{value: "pending"}
	StatusCheckout  = OrderStatus{value: "checkout"}
	StatusCompleted = OrderStatus{value: "completed"}
	StatusExpired   = OrderStatus{value: "expired"}
)

var (
	ErrInvalidOrderStatus = errors.New("invalid order status")
)

var validStatuses = map[string]OrderStatus{
	"pending":   StatusPending,
	"checkout":  StatusCheckout,
	"completed": StatusCompleted,
	"expired":   StatusExpired,
}

// NewOrderStatus creates a new OrderStatus from a string.
func NewOrderStatus(raw string) (OrderStatus, error) {
	if raw == "" {
		return StatusPending, nil
	}

	status, ok := validStatuses[raw]
	if !ok {
		return OrderStatus{}, ErrInvalidOrderStatus
	}

	return status, nil
}

// MustOrderStatus creates a new OrderStatus or panics.
func MustOrderStatus(raw string) OrderStatus {
	s, err := NewOrderStatus(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the status as a string.
func (s OrderStatus) String() string {
	return s.value
}

// IsZero returns true if the status is empty.
func (s OrderStatus) IsZero() bool {
	return s.value == ""
}

// IsPending returns true if the order is pending.
func (s OrderStatus) IsPending() bool {
	return s.value == StatusPending.value
}

// IsCheckout returns true if the order is in checkout.
func (s OrderStatus) IsCheckout() bool {
	return s.value == StatusCheckout.value
}

// IsCompleted returns true if the order is completed.
func (s OrderStatus) IsCompleted() bool {
	return s.value == StatusCompleted.value
}

// IsExpired returns true if the order has expired.
func (s OrderStatus) IsExpired() bool {
	return s.value == StatusExpired.value
}

// CanBeModified returns true if the order status allows modifications.
func (s OrderStatus) CanBeModified() bool {
	return s.IsPending() || s.IsCheckout()
}

// Value implements driver.Valuer for database serialization.
func (s OrderStatus) Value() (driver.Value, error) {
	if s.IsZero() {
		return "pending", nil
	}
	return s.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (s *OrderStatus) Scan(src any) error {
	if src == nil {
		*s = StatusPending
		return nil
	}

	switch v := src.(type) {
	case string:
		status, err := NewOrderStatus(v)
		if err != nil {
			s.value = v
			return nil
		}
		*s = status
		return nil
	case []byte:
		status, err := NewOrderStatus(string(v))
		if err != nil {
			s.value = string(v)
			return nil
		}
		*s = status
		return nil
	default:
		return ErrInvalidOrderStatus
	}
}

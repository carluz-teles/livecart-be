package domain

import (
	"database/sql/driver"
	"errors"
)

// PaymentStatus represents the payment status of an order.
type PaymentStatus struct {
	value string
}

// Payment status constants
var (
	PaymentPending  = PaymentStatus{value: "pending"}
	PaymentPaid     = PaymentStatus{value: "paid"}
	PaymentFailed   = PaymentStatus{value: "failed"}
	PaymentRefunded = PaymentStatus{value: "refunded"}
)

var (
	ErrInvalidPaymentStatus = errors.New("invalid payment status")
)

var validPaymentStatuses = map[string]PaymentStatus{
	"pending":  PaymentPending,
	"paid":     PaymentPaid,
	"failed":   PaymentFailed,
	"refunded": PaymentRefunded,
}

// NewPaymentStatus creates a new PaymentStatus from a string.
func NewPaymentStatus(raw string) (PaymentStatus, error) {
	if raw == "" {
		return PaymentPending, nil
	}

	status, ok := validPaymentStatuses[raw]
	if !ok {
		return PaymentStatus{}, ErrInvalidPaymentStatus
	}

	return status, nil
}

// MustPaymentStatus creates a new PaymentStatus or panics.
func MustPaymentStatus(raw string) PaymentStatus {
	s, err := NewPaymentStatus(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the status as a string.
func (s PaymentStatus) String() string {
	return s.value
}

// IsZero returns true if the status is empty.
func (s PaymentStatus) IsZero() bool {
	return s.value == ""
}

// IsPending returns true if payment is pending.
func (s PaymentStatus) IsPending() bool {
	return s.value == PaymentPending.value
}

// IsPaid returns true if payment has been made.
func (s PaymentStatus) IsPaid() bool {
	return s.value == PaymentPaid.value
}

// IsFailed returns true if payment failed.
func (s PaymentStatus) IsFailed() bool {
	return s.value == PaymentFailed.value
}

// IsRefunded returns true if payment was refunded.
func (s PaymentStatus) IsRefunded() bool {
	return s.value == PaymentRefunded.value
}

// CanBeRefunded returns true if the payment can be refunded.
func (s PaymentStatus) CanBeRefunded() bool {
	return s.IsPaid()
}

// Value implements driver.Valuer for database serialization.
func (s PaymentStatus) Value() (driver.Value, error) {
	if s.IsZero() {
		return "pending", nil
	}
	return s.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (s *PaymentStatus) Scan(src any) error {
	if src == nil {
		*s = PaymentPending
		return nil
	}

	switch v := src.(type) {
	case string:
		status, err := NewPaymentStatus(v)
		if err != nil {
			s.value = v
			return nil
		}
		*s = status
		return nil
	case []byte:
		status, err := NewPaymentStatus(string(v))
		if err != nil {
			s.value = string(v)
			return nil
		}
		*s = status
		return nil
	default:
		return ErrInvalidPaymentStatus
	}
}

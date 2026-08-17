package domain

import (
	"database/sql/driver"
	"errors"
)

// PaymentStatus tracks the payment lifecycle in order_payments.
type PaymentStatus struct{ value string }

var (
	PaymentPending    = PaymentStatus{value: "pending"}
	PaymentPaid       = PaymentStatus{value: "paid"}
	PaymentRefunded   = PaymentStatus{value: "refunded"}
	PaymentFailed     = PaymentStatus{value: "failed"}
	PaymentChargeback = PaymentStatus{value: "chargeback"}
)

var ErrInvalidPaymentStatus = errors.New("invalid payment status")

var validPaymentStatuses = map[string]PaymentStatus{
	"pending":    PaymentPending,
	"paid":       PaymentPaid,
	"refunded":   PaymentRefunded,
	"failed":     PaymentFailed,
	"chargeback": PaymentChargeback,
}

func NewPaymentStatus(raw string) (PaymentStatus, error) {
	if raw == "" {
		return PaymentPending, nil
	}
	s, ok := validPaymentStatuses[raw]
	if !ok {
		return PaymentStatus{}, ErrInvalidPaymentStatus
	}
	return s, nil
}

func (s PaymentStatus) String() string      { return s.value }
func (s PaymentStatus) IsZero() bool        { return s.value == "" }
func (s PaymentStatus) IsPaid() bool        { return s.value == PaymentPaid.value }
func (s PaymentStatus) CanBeRefunded() bool { return s.IsPaid() }

func (s PaymentStatus) Value() (driver.Value, error) {
	if s.IsZero() {
		return "pending", nil
	}
	return s.value, nil
}

func (s *PaymentStatus) Scan(src any) error {
	switch v := src.(type) {
	case string:
		st, err := NewPaymentStatus(v)
		if err != nil {
			s.value = v
			return nil
		}
		*s = st
	case []byte:
		st, err := NewPaymentStatus(string(v))
		if err != nil {
			s.value = string(v)
			return nil
		}
		*s = st
	default:
		return ErrInvalidPaymentStatus
	}
	return nil
}

// Payment holds the payment snapshot and coupon data for an order (1:1 with orders).
type Payment struct {
	orderID             string
	paymentStatus       PaymentStatus
	paymentMethod       *string
	couponID            *string
	couponCode          *string
	couponDiscountCents int64
}

func (p *Payment) OrderID() string            { return p.orderID }
func (p *Payment) Status() PaymentStatus      { return p.paymentStatus }
func (p *Payment) PaymentMethod() *string     { return p.paymentMethod }
func (p *Payment) CouponID() *string          { return p.couponID }
func (p *Payment) CouponCode() *string        { return p.couponCode }
func (p *Payment) CouponDiscountCents() int64 { return p.couponDiscountCents }

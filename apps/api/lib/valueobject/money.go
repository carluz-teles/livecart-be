package valueobject

import (
	"database/sql/driver"
	"fmt"
)

// Money represents a monetary value in cents.
// Using cents avoids floating point precision issues.
type Money struct {
	cents int64
}

// NewMoney creates a new Money from cents.
func NewMoney(cents int64) (Money, error) {
	if cents < 0 {
		return Money{}, ErrNegativeMoney
	}
	return Money{cents: cents}, nil
}

// MustNewMoney creates a new Money or panics if invalid.
func MustNewMoney(cents int64) Money {
	m, err := NewMoney(cents)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero returns a Money with zero value.
func Zero() Money {
	return Money{cents: 0}
}

// FromReais creates Money from a value in reais (e.g., 10.50 -> 1050 cents).
func FromReais(reais float64) (Money, error) {
	cents := int64(reais * 100)
	return NewMoney(cents)
}

// Cents returns the value in cents.
func (m Money) Cents() int64 {
	return m.cents
}

// Reais returns the value in reais as a float.
func (m Money) Reais() float64 {
	return float64(m.cents) / 100
}

// IsZero returns true if the money is zero.
func (m Money) IsZero() bool {
	return m.cents == 0
}

// Equals compares two money values for equality.
func (m Money) Equals(other Money) bool {
	return m.cents == other.cents
}

// Add adds two money values.
func (m Money) Add(other Money) Money {
	return Money{cents: m.cents + other.cents}
}

// Subtract subtracts another money value. Returns error if result would be negative.
func (m Money) Subtract(other Money) (Money, error) {
	result := m.cents - other.cents
	if result < 0 {
		return Money{}, ErrNegativeMoney
	}
	return Money{cents: result}, nil
}

// Multiply multiplies the money by a quantity.
func (m Money) Multiply(quantity int) Money {
	return Money{cents: m.cents * int64(quantity)}
}

// IsGreaterThan returns true if this money is greater than another.
func (m Money) IsGreaterThan(other Money) bool {
	return m.cents > other.cents
}

// IsLessThan returns true if this money is less than another.
func (m Money) IsLessThan(other Money) bool {
	return m.cents < other.cents
}

// String returns the money as a formatted string (e.g., "R$ 10,50").
func (m Money) String() string {
	reais := m.cents / 100
	centavos := m.cents % 100
	return fmt.Sprintf("R$ %d,%02d", reais, centavos)
}

// Value implements driver.Valuer for database serialization.
func (m Money) Value() (driver.Value, error) {
	return m.cents, nil
}

// Scan implements sql.Scanner for database deserialization.
func (m *Money) Scan(src any) error {
	if src == nil {
		m.cents = 0
		return nil
	}

	switch v := src.(type) {
	case int64:
		if v < 0 {
			return ErrNegativeMoney
		}
		m.cents = v
		return nil
	case int:
		if v < 0 {
			return ErrNegativeMoney
		}
		m.cents = int64(v)
		return nil
	case int32:
		if v < 0 {
			return ErrNegativeMoney
		}
		m.cents = int64(v)
		return nil
	case float64:
		cents := int64(v)
		if cents < 0 {
			return ErrNegativeMoney
		}
		m.cents = cents
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Money", src)
	}
}

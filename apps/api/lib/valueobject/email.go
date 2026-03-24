package valueobject

import (
	"database/sql/driver"
	"regexp"
	"strings"
)

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// Email represents a validated email address.
// It is immutable and always normalized (lowercase, trimmed).
type Email struct {
	value string
}

// NewEmail creates a new Email from a raw string.
// Returns an error if the email is invalid.
func NewEmail(raw string) (Email, error) {
	if raw == "" {
		return Email{}, ErrEmptyEmail
	}

	normalized := strings.ToLower(strings.TrimSpace(raw))
	if !emailRegex.MatchString(normalized) {
		return Email{}, ErrInvalidEmail
	}

	return Email{value: normalized}, nil
}

// MustNewEmail creates a new Email or panics if invalid.
// Use only for tests or known-valid values.
func MustNewEmail(raw string) Email {
	e, err := NewEmail(raw)
	if err != nil {
		panic(err)
	}
	return e
}

// String returns the email as a string.
func (e Email) String() string {
	return e.value
}

// IsZero returns true if the email is empty.
func (e Email) IsZero() bool {
	return e.value == ""
}

// Equals compares two emails for equality.
func (e Email) Equals(other Email) bool {
	return e.value == other.value
}

// Domain returns the domain part of the email.
func (e Email) Domain() string {
	parts := strings.Split(e.value, "@")
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// Value implements driver.Valuer for database serialization.
func (e Email) Value() (driver.Value, error) {
	if e.IsZero() {
		return nil, nil
	}
	return e.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (e *Email) Scan(src any) error {
	if src == nil {
		e.value = ""
		return nil
	}

	switch v := src.(type) {
	case string:
		email, err := NewEmail(v)
		if err != nil {
			// If stored value is invalid, still load it but mark as-is
			e.value = v
			return nil
		}
		*e = email
		return nil
	case []byte:
		email, err := NewEmail(string(v))
		if err != nil {
			e.value = string(v)
			return nil
		}
		*e = email
		return nil
	default:
		return ErrInvalidEmail
	}
}

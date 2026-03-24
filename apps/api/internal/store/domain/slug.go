package domain

import (
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
)

var (
	ErrInvalidSlug = errors.New("invalid slug format")
	ErrEmptySlug   = errors.New("slug cannot be empty")
)

var slugRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Slug represents a URL-friendly store identifier.
type Slug struct {
	value string
}

// NewSlug creates a new Slug from a string.
func NewSlug(raw string) (Slug, error) {
	if raw == "" {
		return Slug{}, ErrEmptySlug
	}

	// Normalize to lowercase
	normalized := strings.ToLower(strings.TrimSpace(raw))

	if len(normalized) < 2 || len(normalized) > 50 {
		return Slug{}, ErrInvalidSlug
	}

	if !slugRegex.MatchString(normalized) {
		return Slug{}, ErrInvalidSlug
	}

	return Slug{value: normalized}, nil
}

// MustSlug creates a new Slug or panics.
func MustSlug(raw string) Slug {
	s, err := NewSlug(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the slug as a string.
func (s Slug) String() string {
	return s.value
}

// IsZero returns true if the slug is empty.
func (s Slug) IsZero() bool {
	return s.value == ""
}

// Equals compares two slugs for equality.
func (s Slug) Equals(other Slug) bool {
	return s.value == other.value
}

// Value implements driver.Valuer for database serialization.
func (s Slug) Value() (driver.Value, error) {
	if s.IsZero() {
		return nil, nil
	}
	return s.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (s *Slug) Scan(src any) error {
	if src == nil {
		s.value = ""
		return nil
	}

	switch v := src.(type) {
	case string:
		s.value = v
		return nil
	case []byte:
		s.value = string(v)
		return nil
	default:
		return ErrInvalidSlug
	}
}

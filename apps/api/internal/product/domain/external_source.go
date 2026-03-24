package domain

import (
	"database/sql/driver"
	"errors"
)

// ExternalSource represents the origin/source of a product.
type ExternalSource struct {
	value string
}

// Valid external sources
var (
	ExternalSourceManual  = ExternalSource{value: "manual"}
	ExternalSourceBling   = ExternalSource{value: "bling"}
	ExternalSourceTiny    = ExternalSource{value: "tiny"}
	ExternalSourceShopify = ExternalSource{value: "shopify"}
)

var (
	ErrInvalidExternalSource = errors.New("invalid external source")
)

var validExternalSources = map[string]ExternalSource{
	"manual":  ExternalSourceManual,
	"bling":   ExternalSourceBling,
	"tiny":    ExternalSourceTiny,
	"shopify": ExternalSourceShopify,
}

// NewExternalSource creates a new ExternalSource from a string.
func NewExternalSource(raw string) (ExternalSource, error) {
	if raw == "" {
		return ExternalSourceManual, nil // default to manual
	}

	source, ok := validExternalSources[raw]
	if !ok {
		return ExternalSource{}, ErrInvalidExternalSource
	}

	return source, nil
}

// MustExternalSource creates a new ExternalSource or panics.
func MustExternalSource(raw string) ExternalSource {
	s, err := NewExternalSource(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the external source as a string.
func (e ExternalSource) String() string {
	return e.value
}

// IsZero returns true if the external source is empty.
func (e ExternalSource) IsZero() bool {
	return e.value == ""
}

// IsManual returns true if the source is manual.
func (e ExternalSource) IsManual() bool {
	return e.value == ExternalSourceManual.value
}

// IsExternal returns true if the product comes from an external system.
func (e ExternalSource) IsExternal() bool {
	return !e.IsManual() && !e.IsZero()
}

// Equals checks if two external sources are equal.
func (e ExternalSource) Equals(other ExternalSource) bool {
	return e.value == other.value
}

// Value implements driver.Valuer for database serialization.
func (e ExternalSource) Value() (driver.Value, error) {
	if e.IsZero() {
		return "manual", nil
	}
	return e.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (e *ExternalSource) Scan(src any) error {
	if src == nil {
		*e = ExternalSourceManual
		return nil
	}

	switch v := src.(type) {
	case string:
		source, err := NewExternalSource(v)
		if err != nil {
			e.value = v
			return nil
		}
		*e = source
		return nil
	case []byte:
		source, err := NewExternalSource(string(v))
		if err != nil {
			e.value = string(v)
			return nil
		}
		*e = source
		return nil
	default:
		return ErrInvalidExternalSource
	}
}

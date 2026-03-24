package product

import (
	"fmt"
	"strconv"
)

const (
	// KeywordMin is the minimum valid keyword value
	KeywordMin = 1000
	// KeywordMax is the maximum valid keyword value
	KeywordMax = 9999
)

// Keyword is a value object representing a product keyword.
// Keywords are 4-digit numeric strings between 1000-9999.
type Keyword struct {
	value string
}

// NewKeyword creates a new Keyword from a string value.
// Returns an error if the value is invalid.
func NewKeyword(value string) (Keyword, error) {
	if value == "" {
		return Keyword{}, fmt.Errorf("keyword cannot be empty")
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return Keyword{}, fmt.Errorf("keyword must be numeric: %w", err)
	}

	if n < KeywordMin || n > KeywordMax {
		return Keyword{}, fmt.Errorf("keyword must be between %d and %d", KeywordMin, KeywordMax)
	}

	// Normalize to 4 digits with leading zeros
	return Keyword{value: fmt.Sprintf("%04d", n)}, nil
}

// MustKeyword creates a new Keyword or panics if invalid.
// Use only for values known to be valid (e.g., from database).
func MustKeyword(value string) Keyword {
	kw, err := NewKeyword(value)
	if err != nil {
		panic(err)
	}
	return kw
}

// NextKeyword generates the next keyword after the given current value.
// If current is empty or invalid, starts from KeywordMin.
func NextKeyword(current string) (Keyword, error) {
	var n int

	if current == "" {
		n = KeywordMin - 1
	} else {
		var err error
		n, err = strconv.Atoi(current)
		if err != nil {
			// Invalid current, start from min
			n = KeywordMin - 1
		}
	}

	next := n + 1
	if next < KeywordMin {
		next = KeywordMin
	}
	if next > KeywordMax {
		return Keyword{}, fmt.Errorf("keyword range exhausted (max %d)", KeywordMax)
	}

	return Keyword{value: fmt.Sprintf("%04d", next)}, nil
}

// String returns the keyword value as a string.
func (k Keyword) String() string {
	return k.value
}

// Int returns the keyword value as an integer.
func (k Keyword) Int() int {
	n, _ := strconv.Atoi(k.value)
	return n
}

// IsZero returns true if the keyword is empty/unset.
func (k Keyword) IsZero() bool {
	return k.value == ""
}

// Equals checks if two keywords are equal.
func (k Keyword) Equals(other Keyword) bool {
	return k.value == other.value
}

// IsValid checks if a string would be a valid keyword.
func IsValidKeyword(value string) bool {
	_, err := NewKeyword(value)
	return err == nil
}

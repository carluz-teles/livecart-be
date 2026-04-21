package domain

import "errors"

// Domain errors for cart settings
var (
	ErrInvalidExpirationMinutes    = errors.New("expiration minutes must be 0 or positive")
	ErrInvalidMaxQuantityPerItem   = errors.New("max quantity per item must be 0 or positive")
)

// CartSettings represents the cart configuration for a store.
type CartSettings struct {
	enabled            bool
	expirationMinutes  int
	reserveStock       bool
	maxQuantityPerItem int
}

// DefaultCartSettings returns the default cart settings for a new store.
func DefaultCartSettings() CartSettings {
	return CartSettings{
		enabled:            true,
		expirationMinutes:  30,
		reserveStock:       true,
		maxQuantityPerItem: 5,
	}
}

// NewCartSettings creates a new CartSettings with validation.
func NewCartSettings(
	enabled bool,
	expirationMinutes int,
	reserveStock bool,
	maxQuantityPerItem int,
) (CartSettings, error) {
	if expirationMinutes < 0 {
		return CartSettings{}, ErrInvalidExpirationMinutes
	}
	if maxQuantityPerItem < 0 {
		return CartSettings{}, ErrInvalidMaxQuantityPerItem
	}

	return CartSettings{
		enabled:            enabled,
		expirationMinutes:  expirationMinutes,
		reserveStock:       reserveStock,
		maxQuantityPerItem: maxQuantityPerItem,
	}, nil
}

// ReconstructCartSettings rebuilds CartSettings from persistence data.
func ReconstructCartSettings(
	enabled bool,
	expirationMinutes int,
	reserveStock bool,
	maxQuantityPerItem int,
) CartSettings {
	return CartSettings{
		enabled:            enabled,
		expirationMinutes:  expirationMinutes,
		reserveStock:       reserveStock,
		maxQuantityPerItem: maxQuantityPerItem,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (c CartSettings) Enabled() bool           { return c.enabled }
func (c CartSettings) ExpirationMinutes() int  { return c.expirationMinutes }
func (c CartSettings) ReserveStock() bool      { return c.reserveStock }
func (c CartSettings) MaxQuantityPerItem() int { return c.maxQuantityPerItem }

// ============================================
// Business Rules
// ============================================

// HasExpiration returns true if the cart has an expiration time.
func (c CartSettings) HasExpiration() bool {
	return c.expirationMinutes > 0
}

// HasQuantityLimit returns true if there's a limit on quantity per item.
func (c CartSettings) HasQuantityLimit() bool {
	return c.maxQuantityPerItem > 0
}

// IsWithinQuantityLimit checks if adding quantity would exceed the limit.
func (c CartSettings) IsWithinQuantityLimit(currentQuantity, addQuantity int) bool {
	if !c.HasQuantityLimit() {
		return true
	}
	return currentQuantity+addQuantity <= c.maxQuantityPerItem
}

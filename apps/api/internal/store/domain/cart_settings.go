package domain

import "errors"

// Domain errors for cart settings
var (
	ErrInvalidExpirationMinutes  = errors.New("expiration minutes must be 0 or positive")
	ErrInvalidMaxQuantityPerItem = errors.New("max quantity per item must be 0 or positive")
)

// CartSettings represents the cart configuration for a store. It carries the
// full persisted shape (base rules + automatic-message settings) so the
// presentation layer can render the whole cartSettings object from getters.
type CartSettings struct {
	enabled             bool
	expirationMinutes   int
	reserveStock        bool
	allowStorePickup    bool
	maxQuantityPerItem  int
	// minInstallmentCents é o piso de uma parcela no cartão, em centavos.
	// Zero = sem mínimo.
	minInstallmentCents int
	allowEdit           bool
	checkoutSendMethods []string
	// Automatic message settings
	realTimeCart              bool
	sendOnLiveEnd             bool
	messageCooldownSeconds    int
	sendExpirationReminder    bool
	expirationReminderMinutes int
}

// DefaultCartSettings returns the default cart settings for a new store.
func DefaultCartSettings() CartSettings {
	return CartSettings{
		enabled:             true,
		expirationMinutes:   30,
		reserveStock:        true,
		maxQuantityPerItem:  5,
		checkoutSendMethods: []string{"public_link", "manual"},
	}
}

// NewCartSettings creates a new CartSettings with validation of the base rules.
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

// ReconstructCartSettings rebuilds the full CartSettings from persistence data
// (no validation — the row is trusted).
func ReconstructCartSettings(
	enabled bool,
	expirationMinutes int,
	reserveStock bool,
	allowStorePickup bool,
	maxQuantityPerItem int,
	minInstallmentCents int,
	allowEdit bool,
	checkoutSendMethods []string,
	realTimeCart bool,
	sendOnLiveEnd bool,
	messageCooldownSeconds int,
	sendExpirationReminder bool,
	expirationReminderMinutes int,
) CartSettings {
	return CartSettings{
		enabled:                   enabled,
		expirationMinutes:         expirationMinutes,
		reserveStock:              reserveStock,
		allowStorePickup:          allowStorePickup,
		maxQuantityPerItem:        maxQuantityPerItem,
		minInstallmentCents:       minInstallmentCents,
		allowEdit:                 allowEdit,
		checkoutSendMethods:       checkoutSendMethods,
		realTimeCart:              realTimeCart,
		sendOnLiveEnd:             sendOnLiveEnd,
		messageCooldownSeconds:    messageCooldownSeconds,
		sendExpirationReminder:    sendExpirationReminder,
		expirationReminderMinutes: expirationReminderMinutes,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (c CartSettings) Enabled() bool                  { return c.enabled }
func (c CartSettings) ExpirationMinutes() int         { return c.expirationMinutes }
func (c CartSettings) ReserveStock() bool             { return c.reserveStock }
func (c CartSettings) AllowStorePickup() bool         { return c.allowStorePickup }
func (c CartSettings) MaxQuantityPerItem() int        { return c.maxQuantityPerItem }
func (c CartSettings) MinInstallmentCents() int       { return c.minInstallmentCents }
func (c CartSettings) AllowEdit() bool                { return c.allowEdit }
func (c CartSettings) CheckoutSendMethods() []string  { return c.checkoutSendMethods }
func (c CartSettings) RealTimeCart() bool             { return c.realTimeCart }
func (c CartSettings) SendOnLiveEnd() bool            { return c.sendOnLiveEnd }
func (c CartSettings) MessageCooldownSeconds() int    { return c.messageCooldownSeconds }
func (c CartSettings) SendExpirationReminder() bool   { return c.sendExpirationReminder }
func (c CartSettings) ExpirationReminderMinutes() int { return c.expirationReminderMinutes }

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

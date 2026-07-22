// Package domain holds the customer aggregate: the entity the service returns
// and the presentation layer maps to a response. Fields are private with
// immutable getters; Reconstruct rebuilds an instance from persistence without
// running any invariant (data already validated on write).
package domain

import (
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

// ShippingAddress is the last delivery destination captured at checkout. It is
// a value object embedded in the Customer aggregate for the detail view.
type ShippingAddress struct {
	zipCode      string
	street       string
	number       string
	complement   string
	neighborhood string
	city         string
	state        string
}

// NewShippingAddress builds a ShippingAddress value object.
func NewShippingAddress(zipCode, street, number, complement, neighborhood, city, state string) ShippingAddress {
	return ShippingAddress{
		zipCode:      zipCode,
		street:       street,
		number:       number,
		complement:   complement,
		neighborhood: neighborhood,
		city:         city,
		state:        state,
	}
}

func (a ShippingAddress) ZipCode() string      { return a.zipCode }
func (a ShippingAddress) Street() string       { return a.street }
func (a ShippingAddress) Number() string       { return a.number }
func (a ShippingAddress) Complement() string   { return a.complement }
func (a ShippingAddress) Neighborhood() string { return a.neighborhood }
func (a ShippingAddress) City() string         { return a.city }
func (a ShippingAddress) State() string        { return a.state }

// Customer represents a buyer of a store, aggregated with order stats and the
// latest checkout snapshot (name, document, shipping address).
type Customer struct {
	id                  vo.CustomerID
	platformUserID      string
	handle              string
	email               *string
	phone               *string
	name                *string
	document            *string
	totalOrders         int
	totalSpent          int64
	lastOrderAt         *time.Time
	firstOrderAt        *time.Time
	lastShippingAddress *ShippingAddress
}

// Reconstruct rebuilds a Customer from persistence data. No validation: the
// row is assumed consistent.
func Reconstruct(
	id vo.CustomerID,
	platformUserID string,
	handle string,
	email *string,
	phone *string,
	name *string,
	document *string,
	totalOrders int,
	totalSpent int64,
	lastOrderAt *time.Time,
	firstOrderAt *time.Time,
	lastShippingAddress *ShippingAddress,
) *Customer {
	return &Customer{
		id:                  id,
		platformUserID:      platformUserID,
		handle:              handle,
		email:               email,
		phone:               phone,
		name:                name,
		document:            document,
		totalOrders:         totalOrders,
		totalSpent:          totalSpent,
		lastOrderAt:         lastOrderAt,
		firstOrderAt:        firstOrderAt,
		lastShippingAddress: lastShippingAddress,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (c *Customer) ID() vo.CustomerID                     { return c.id }
func (c *Customer) PlatformUserID() string                { return c.platformUserID }
func (c *Customer) Handle() string                        { return c.handle }
func (c *Customer) Email() *string                        { return c.email }
func (c *Customer) Phone() *string                        { return c.phone }
func (c *Customer) Name() *string                         { return c.name }
func (c *Customer) Document() *string                     { return c.document }
func (c *Customer) TotalOrders() int                      { return c.totalOrders }
func (c *Customer) TotalSpent() int64                     { return c.totalSpent }
func (c *Customer) LastOrderAt() *time.Time               { return c.lastOrderAt }
func (c *Customer) FirstOrderAt() *time.Time              { return c.firstOrderAt }
func (c *Customer) LastShippingAddress() *ShippingAddress { return c.lastShippingAddress }

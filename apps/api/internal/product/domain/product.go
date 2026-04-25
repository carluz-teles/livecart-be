package domain

import (
	"errors"
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

const (
	// LowStockThreshold is the threshold below which stock is considered low
	LowStockThreshold = 5
)

// PackageFormat describes the shape hint sent to shipping carriers.
type PackageFormat string

const (
	PackageFormatBox    PackageFormat = "box"
	PackageFormatRoll   PackageFormat = "roll"
	PackageFormatLetter PackageFormat = "letter"
)

var ErrInvalidPackageFormat = errors.New("package format must be box, roll or letter")

func NewPackageFormat(s string) (PackageFormat, error) {
	if s == "" {
		return PackageFormatBox, nil
	}
	switch PackageFormat(s) {
	case PackageFormatBox, PackageFormatRoll, PackageFormatLetter:
		return PackageFormat(s), nil
	default:
		return "", ErrInvalidPackageFormat
	}
}

func (f PackageFormat) String() string { return string(f) }

// Domain errors
var (
	ErrProductNameRequired = errors.New("product name is required")
	ErrInvalidStock        = errors.New("stock cannot be negative")
	ErrInvalidPrice        = errors.New("price cannot be negative")
	ErrInvalidDimensions   = errors.New("weight and dimensions must all be provided together and be positive")
)

// ShippingProfile holds the physical attributes needed to quote freight.
// All dimension fields are optional: when any is nil, the product is not shippable yet.
type ShippingProfile struct {
	WeightGrams         *int
	HeightCm            *int
	WidthCm             *int
	LengthCm            *int
	SKU                 string
	PackageFormat       PackageFormat
	InsuranceValueCents *int64
}

// IsComplete reports whether the profile has enough data to quote shipping.
func (s ShippingProfile) IsComplete() bool {
	return s.WeightGrams != nil && *s.WeightGrams > 0 &&
		s.HeightCm != nil && *s.HeightCm > 0 &&
		s.WidthCm != nil && *s.WidthCm > 0 &&
		s.LengthCm != nil && *s.LengthCm > 0
}

// Product represents a product in the store.
type Product struct {
	id             vo.ProductID
	storeID        vo.StoreID
	groupID        *vo.ID // optional: links variants to their product_group aggregator
	name           string
	externalID     string
	externalSource ExternalSource
	keyword        Keyword
	price          vo.Money
	imageURL       string
	stock          int
	active         bool
	shipping       ShippingProfile
	createdAt      time.Time
	updatedAt      time.Time
}

// NewProduct creates a new Product entity.
func NewProduct(
	storeID vo.StoreID,
	name string,
	externalID string,
	externalSource ExternalSource,
	keyword Keyword,
	price vo.Money,
	imageURL string,
	stock int,
	shipping ShippingProfile,
) (*Product, error) {
	if name == "" {
		return nil, ErrProductNameRequired
	}

	if stock < 0 {
		return nil, ErrInvalidStock
	}

	if err := validateShipping(shipping); err != nil {
		return nil, err
	}

	if shipping.PackageFormat == "" {
		shipping.PackageFormat = PackageFormatBox
	}

	now := time.Now()
	return &Product{
		id:             vo.GenerateProductID(),
		storeID:        storeID,
		name:           name,
		externalID:     externalID,
		externalSource: externalSource,
		keyword:        keyword,
		price:          price,
		imageURL:       imageURL,
		stock:          stock,
		active:         true, // new products are active by default
		shipping:       shipping,
		createdAt:      now,
		updatedAt:      now,
	}, nil
}

// Reconstruct rebuilds a Product from persistence data.
func Reconstruct(
	id vo.ProductID,
	storeID vo.StoreID,
	name string,
	externalID string,
	externalSource ExternalSource,
	keyword Keyword,
	price vo.Money,
	imageURL string,
	stock int,
	active bool,
	shipping ShippingProfile,
	createdAt time.Time,
	updatedAt time.Time,
) *Product {
	if shipping.PackageFormat == "" {
		shipping.PackageFormat = PackageFormatBox
	}
	return &Product{
		id:             id,
		storeID:        storeID,
		name:           name,
		externalID:     externalID,
		externalSource: externalSource,
		keyword:        keyword,
		price:          price,
		imageURL:       imageURL,
		stock:          stock,
		active:         active,
		shipping:       shipping,
		createdAt:      createdAt,
		updatedAt:      updatedAt,
	}
}

// AttachGroup links the product to a product group (variant of an aggregator).
// Pass nil to detach.
func (p *Product) AttachGroup(groupID *vo.ID) {
	p.groupID = groupID
	p.updatedAt = time.Now()
}

// validateShipping enforces: dimensions are all-or-nothing, each positive when set.
func validateShipping(s ShippingProfile) error {
	set := 0
	if s.WeightGrams != nil {
		if *s.WeightGrams <= 0 {
			return ErrInvalidDimensions
		}
		set++
	}
	if s.HeightCm != nil {
		if *s.HeightCm <= 0 {
			return ErrInvalidDimensions
		}
		set++
	}
	if s.WidthCm != nil {
		if *s.WidthCm <= 0 {
			return ErrInvalidDimensions
		}
		set++
	}
	if s.LengthCm != nil {
		if *s.LengthCm <= 0 {
			return ErrInvalidDimensions
		}
		set++
	}
	if set != 0 && set != 4 {
		return ErrInvalidDimensions
	}
	if s.InsuranceValueCents != nil && *s.InsuranceValueCents < 0 {
		return ErrInvalidPrice
	}
	return nil
}

// ============================================
// Getters (immutable access)
// ============================================

func (p *Product) ID() vo.ProductID                { return p.id }
func (p *Product) StoreID() vo.StoreID             { return p.storeID }
func (p *Product) GroupID() *vo.ID                 { return p.groupID }
func (p *Product) Name() string                    { return p.name }
func (p *Product) ExternalID() string              { return p.externalID }
func (p *Product) ExternalSource() ExternalSource  { return p.externalSource }
func (p *Product) Keyword() Keyword                { return p.keyword }
func (p *Product) Price() vo.Money                 { return p.price }
func (p *Product) ImageURL() string                { return p.imageURL }
func (p *Product) Stock() int                      { return p.stock }
func (p *Product) Active() bool                    { return p.active }
func (p *Product) Shipping() ShippingProfile       { return p.shipping }
func (p *Product) CreatedAt() time.Time            { return p.createdAt }
func (p *Product) UpdatedAt() time.Time            { return p.updatedAt }

// IsShippable reports whether the product has the physical data required for carriers.
func (p *Product) IsShippable() bool { return p.shipping.IsComplete() }

// ============================================
// Business Rules
// ============================================

// IsActive returns true if the product is active.
func (p *Product) IsActive() bool {
	return p.active
}

// HasLowStock returns true if the product stock is below the threshold.
func (p *Product) HasLowStock() bool {
	return p.stock <= LowStockThreshold
}

// IsOutOfStock returns true if the product has no stock.
func (p *Product) IsOutOfStock() bool {
	return p.stock == 0
}

// IsFromExternalSource returns true if the product was imported.
func (p *Product) IsFromExternalSource() bool {
	return p.externalSource.IsExternal()
}

// StockValue returns the total value of stock (price * stock).
func (p *Product) StockValue() vo.Money {
	return p.price.Multiply(p.stock)
}

// CanBeOrdered returns true if the product can be added to an order.
func (p *Product) CanBeOrdered() bool {
	return p.active && p.stock > 0
}

// HasSufficientStock checks if there's enough stock for the quantity.
func (p *Product) HasSufficientStock(quantity int) bool {
	return p.stock >= quantity
}

// ============================================
// State Changes
// ============================================

// UpdateDetails updates the product's basic information and shipping profile.
func (p *Product) UpdateDetails(name string, price vo.Money, imageURL string, stock int, active bool, shipping ShippingProfile) error {
	if name == "" {
		return ErrProductNameRequired
	}

	if stock < 0 {
		return ErrInvalidStock
	}

	if err := validateShipping(shipping); err != nil {
		return err
	}

	if shipping.PackageFormat == "" {
		shipping.PackageFormat = PackageFormatBox
	}

	p.name = name
	p.price = price
	p.imageURL = imageURL
	p.stock = stock
	p.active = active
	p.shipping = shipping
	p.updatedAt = time.Now()

	return nil
}

// Activate activates the product.
func (p *Product) Activate() {
	p.active = true
	p.updatedAt = time.Now()
}

// Deactivate deactivates the product.
func (p *Product) Deactivate() {
	p.active = false
	p.updatedAt = time.Now()
}

// AddStock increases the product stock.
func (p *Product) AddStock(quantity int) {
	if quantity > 0 {
		p.stock += quantity
		p.updatedAt = time.Now()
	}
}

// RemoveStock decreases the product stock.
// Returns error if insufficient stock.
func (p *Product) RemoveStock(quantity int) error {
	if quantity > p.stock {
		return ErrInvalidStock
	}

	p.stock -= quantity
	p.updatedAt = time.Now()
	return nil
}

// UpdatePrice updates the product price.
func (p *Product) UpdatePrice(price vo.Money) error {
	if price.IsNegative() {
		return ErrInvalidPrice
	}

	p.price = price
	p.updatedAt = time.Now()
	return nil
}

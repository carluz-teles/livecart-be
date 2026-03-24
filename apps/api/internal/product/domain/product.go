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

// Domain errors
var (
	ErrProductNameRequired = errors.New("product name is required")
	ErrInvalidStock        = errors.New("stock cannot be negative")
	ErrInvalidPrice        = errors.New("price cannot be negative")
)

// Product represents a product in the store.
type Product struct {
	id             vo.ProductID
	storeID        vo.StoreID
	name           string
	externalID     string
	externalSource ExternalSource
	keyword        Keyword
	price          vo.Money
	imageURL       string
	stock          int
	active         bool
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
) (*Product, error) {
	if name == "" {
		return nil, ErrProductNameRequired
	}

	if stock < 0 {
		return nil, ErrInvalidStock
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
	createdAt time.Time,
	updatedAt time.Time,
) *Product {
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
		createdAt:      createdAt,
		updatedAt:      updatedAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (p *Product) ID() vo.ProductID          { return p.id }
func (p *Product) StoreID() vo.StoreID       { return p.storeID }
func (p *Product) Name() string              { return p.name }
func (p *Product) ExternalID() string        { return p.externalID }
func (p *Product) ExternalSource() ExternalSource { return p.externalSource }
func (p *Product) Keyword() Keyword          { return p.keyword }
func (p *Product) Price() vo.Money           { return p.price }
func (p *Product) ImageURL() string          { return p.imageURL }
func (p *Product) Stock() int                { return p.stock }
func (p *Product) Active() bool              { return p.active }
func (p *Product) CreatedAt() time.Time      { return p.createdAt }
func (p *Product) UpdatedAt() time.Time      { return p.updatedAt }

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

// UpdateDetails updates the product's basic information.
func (p *Product) UpdateDetails(name string, price vo.Money, imageURL string, stock int, active bool) error {
	if name == "" {
		return ErrProductNameRequired
	}

	if stock < 0 {
		return ErrInvalidStock
	}

	p.name = name
	p.price = price
	p.imageURL = imageURL
	p.stock = stock
	p.active = active
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

package domain

import (
	"errors"
	"time"

	productdomain "livecart/apps/api/internal/product/domain"
	vo "livecart/apps/api/lib/valueobject"
)

var (
	ErrGroupNameRequired      = errors.New("group name is required")
	ErrOptionsRequired        = errors.New("at least one option is required")
	ErrOptionNameRequired     = errors.New("option name is required")
	ErrOptionValuesRequired   = errors.New("each option must have at least one value")
	ErrVariantsRequired       = errors.New("at least one variant is required")
	ErrVariantOptionsMismatch = errors.New("variant optionValues length must match number of options")
	ErrUnknownOptionValue     = errors.New("variant references unknown option value")
	ErrDuplicateVariant       = errors.New("two variants share the same option value combination")
)

// Group is the catalog aggregator for a product with variants.
type Group struct {
	id             vo.ID
	storeID        vo.StoreID
	name           string
	description    string
	externalID     string
	externalSource productdomain.ExternalSource
	createdAt      time.Time
	updatedAt      time.Time
}

func NewGroup(storeID vo.StoreID, name, description, externalID string, externalSource productdomain.ExternalSource) (*Group, error) {
	if name == "" {
		return nil, ErrGroupNameRequired
	}
	now := time.Now()
	return &Group{
		id:             vo.GenerateID(),
		storeID:        storeID,
		name:           name,
		description:    description,
		externalID:     externalID,
		externalSource: externalSource,
		createdAt:      now,
		updatedAt:      now,
	}, nil
}

func Reconstruct(id vo.ID, storeID vo.StoreID, name, description, externalID string, externalSource productdomain.ExternalSource, createdAt, updatedAt time.Time) *Group {
	return &Group{
		id: id, storeID: storeID, name: name, description: description,
		externalID: externalID, externalSource: externalSource,
		createdAt: createdAt, updatedAt: updatedAt,
	}
}

func (g *Group) ID() vo.ID                                 { return g.id }
func (g *Group) StoreID() vo.StoreID                       { return g.storeID }
func (g *Group) Name() string                              { return g.name }
func (g *Group) Description() string                       { return g.description }
func (g *Group) ExternalID() string                        { return g.externalID }
func (g *Group) ExternalSource() productdomain.ExternalSource { return g.externalSource }
func (g *Group) CreatedAt() time.Time                      { return g.createdAt }
func (g *Group) UpdatedAt() time.Time                      { return g.updatedAt }

func (g *Group) Update(name, description string) error {
	if name == "" {
		return ErrGroupNameRequired
	}
	g.name = name
	g.description = description
	g.updatedAt = time.Now()
	return nil
}

// Option is a variation dimension (Color, Size, ...).
type Option struct {
	ID       vo.ID
	GroupID  vo.ID
	Name     string
	Position int
	Values   []OptionValue
}

// OptionValue is one allowed value for an Option (Red, Blue / S, M, L).
type OptionValue struct {
	ID       vo.ID
	OptionID vo.ID
	Value    string
	Position int
}

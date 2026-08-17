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

func (g *Group) ID() vo.ID                                    { return g.id }
func (g *Group) StoreID() vo.StoreID                          { return g.storeID }
func (g *Group) Name() string                                 { return g.name }
func (g *Group) Description() string                          { return g.description }
func (g *Group) ExternalID() string                           { return g.externalID }
func (g *Group) ExternalSource() productdomain.ExternalSource { return g.externalSource }
func (g *Group) CreatedAt() time.Time                         { return g.createdAt }
func (g *Group) UpdatedAt() time.Time                         { return g.updatedAt }

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

// Image is a gallery image belonging to a group or a variant.
type Image struct {
	ID       string
	URL      string
	Position int
}

// OptionValuePair is a denormalized (option, value) pair a variant is bound to.
type OptionValuePair struct {
	Option string
	Value  string
}

// Variant is a concrete purchasable product under a group.
type Variant struct {
	ID           string
	Keyword      string
	OptionValues []OptionValuePair
	Price        int64
	Stock        int
	SKU          string
	ImageURL     string
	Images       []Image
}

// Detail is the fully-loaded read model of a group: the aggregator plus its
// options, gallery and variants. It is a domain aggregate assembled by the
// repository/service from several rows; the presentation layer maps it to a
// Response via NewGroupDetailResponse.
type Detail struct {
	group       *Group
	options     []Option
	groupImages []Image
	variants    []Variant
}

// NewDetail assembles the read aggregate from its already-loaded parts.
func NewDetail(group *Group, options []Option, groupImages []Image, variants []Variant) *Detail {
	return &Detail{group: group, options: options, groupImages: groupImages, variants: variants}
}

func (d *Detail) Group() *Group        { return d.group }
func (d *Detail) Options() []Option    { return d.options }
func (d *Detail) GroupImages() []Image { return d.groupImages }
func (d *Detail) Variants() []Variant  { return d.variants }

// CreatedVariant is the minimal identity of a variant produced by a Create.
type CreatedVariant struct {
	ID           string
	Keyword      string
	OptionValues []string
}

// CreateResult is what Create returns: the new group identity plus the created
// variants. It is a domain-side result object (no HTTP concerns).
type CreateResult struct {
	id        string
	name      string
	variants  []CreatedVariant
	createdAt time.Time
}

// NewCreateResult builds the create result from the persisted identities.
func NewCreateResult(id, name string, variants []CreatedVariant, createdAt time.Time) *CreateResult {
	return &CreateResult{id: id, name: name, variants: variants, createdAt: createdAt}
}

func (r *CreateResult) ID() string                 { return r.id }
func (r *CreateResult) Name() string               { return r.name }
func (r *CreateResult) Variants() []CreatedVariant { return r.variants }
func (r *CreateResult) CreatedAt() time.Time       { return r.createdAt }

// Summary is a lightweight list-row view of a group.
type Summary struct {
	ID             string
	Name           string
	Description    string
	ExternalID     string
	ExternalSource string
	VariantsCount  int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

package domain

import (
	"errors"
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

// Domain errors
var (
	ErrStoreNameRequired = errors.New("store name is required")
	ErrStoreNameTooShort = errors.New("store name must be at least 2 characters")
	ErrStoreNameTooLong  = errors.New("store name must be at most 100 characters")
)

// Store represents a store in the system. It carries the full persisted state
// so the presentation layer can render the whole store response from getters —
// the domain never knows about the response DTO.
type Store struct {
	id               vo.StoreID
	name             string
	slug             Slug
	active           bool
	whatsappNumber   *string
	emailAddress     *string
	smsNumber        *string
	description      *string
	website          *string
	logoURL          *string
	cnpj             *string
	address          Address
	cartSettings     CartSettings
	shippingDefaults ShippingDefaults
	createdAt        time.Time
	updatedAt        time.Time
}

// NewStore creates a new Store entity.
func NewStore(name string, slug Slug) (*Store, error) {
	if name == "" {
		return nil, ErrStoreNameRequired
	}
	if len(name) < 2 {
		return nil, ErrStoreNameTooShort
	}
	if len(name) > 100 {
		return nil, ErrStoreNameTooLong
	}

	now := time.Now()
	return &Store{
		id:           vo.GenerateStoreID(),
		name:         name,
		slug:         slug,
		active:       true, // new stores are active by default
		cartSettings: DefaultCartSettings(),
		createdAt:    now,
		updatedAt:    now,
	}, nil
}

// Reconstruct rebuilds a Store from persistence data (trusted; no validation).
func Reconstruct(
	id vo.StoreID,
	name string,
	slug Slug,
	active bool,
	whatsappNumber *string,
	emailAddress *string,
	smsNumber *string,
	description *string,
	website *string,
	logoURL *string,
	cnpj *string,
	address Address,
	cartSettings CartSettings,
	shippingDefaults ShippingDefaults,
	createdAt time.Time,
	updatedAt time.Time,
) *Store {
	return &Store{
		id:               id,
		name:             name,
		slug:             slug,
		active:           active,
		whatsappNumber:   whatsappNumber,
		emailAddress:     emailAddress,
		smsNumber:        smsNumber,
		description:      description,
		website:          website,
		logoURL:          logoURL,
		cnpj:             cnpj,
		address:          address,
		cartSettings:     cartSettings,
		shippingDefaults: shippingDefaults,
		createdAt:        createdAt,
		updatedAt:        updatedAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (s *Store) ID() vo.StoreID                     { return s.id }
func (s *Store) Name() string                       { return s.name }
func (s *Store) Slug() Slug                         { return s.slug }
func (s *Store) Active() bool                       { return s.active }
func (s *Store) WhatsappNumber() *string            { return s.whatsappNumber }
func (s *Store) EmailAddress() *string              { return s.emailAddress }
func (s *Store) SMSNumber() *string                 { return s.smsNumber }
func (s *Store) Description() *string               { return s.description }
func (s *Store) Website() *string                   { return s.website }
func (s *Store) LogoURL() *string                   { return s.logoURL }
func (s *Store) CNPJ() *string                      { return s.cnpj }
func (s *Store) Address() Address                   { return s.address }
func (s *Store) CartSettings() CartSettings         { return s.cartSettings }
func (s *Store) ShippingDefaults() ShippingDefaults { return s.shippingDefaults }
func (s *Store) CreatedAt() time.Time               { return s.createdAt }
func (s *Store) UpdatedAt() time.Time               { return s.updatedAt }

// SetLogoURL overrides the logo URL in-memory (used by the controller to swap
// the stored S3 key for a presigned URL before rendering the response).
func (s *Store) SetLogoURL(url *string) { s.logoURL = url }

// ============================================
// Business Rules
// ============================================

// IsActive returns true if the store is active.
func (s *Store) IsActive() bool {
	return s.active
}

// HasWhatsapp returns true if the store has a WhatsApp number configured.
func (s *Store) HasWhatsapp() bool {
	return s.whatsappNumber != nil && *s.whatsappNumber != ""
}

// HasEmail returns true if the store has an email address configured.
func (s *Store) HasEmail() bool {
	return s.emailAddress != nil && *s.emailAddress != ""
}

// HasSMS returns true if the store has an SMS number configured.
func (s *Store) HasSMS() bool {
	return s.smsNumber != nil && *s.smsNumber != ""
}

// ============================================
// State Changes
// ============================================

// UpdateDetails updates the store's basic information.
func (s *Store) UpdateDetails(name string, whatsappNumber, emailAddress, smsNumber *string) error {
	if name == "" {
		return ErrStoreNameRequired
	}
	if len(name) < 2 {
		return ErrStoreNameTooShort
	}
	if len(name) > 100 {
		return ErrStoreNameTooLong
	}

	s.name = name
	s.whatsappNumber = whatsappNumber
	s.emailAddress = emailAddress
	s.smsNumber = smsNumber
	s.updatedAt = time.Now()

	return nil
}

// Activate activates the store.
func (s *Store) Activate() {
	s.active = true
	s.updatedAt = time.Now()
}

// Deactivate deactivates the store.
func (s *Store) Deactivate() {
	s.active = false
	s.updatedAt = time.Now()
}

// UpdateCartSettings updates the store's cart configuration.
func (s *Store) UpdateCartSettings(settings CartSettings) {
	s.cartSettings = settings
	s.updatedAt = time.Now()
}

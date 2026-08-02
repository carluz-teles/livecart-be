package store

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	storedomain "livecart/apps/api/internal/store/domain"
	appvalidation "livecart/apps/api/lib/validation"
)

// ============================================
// Cart Settings Types
// ============================================

type CartSettingsDTO struct {
	Enabled             bool     `json:"enabled"`
	ExpirationMinutes   int      `json:"expirationMinutes"`
	ReserveStock        bool     `json:"reserveStock"`
	AllowStorePickup    bool     `json:"allowStorePickup"`
	MaxQuantityPerItem  int      `json:"maxQuantityPerItem"`
	AllowEdit           bool     `json:"allowEdit"`
	CheckoutSendMethods []string `json:"checkoutSendMethods"`
	// Automatic message settings
	RealTimeCart              bool `json:"realTimeCart"`
	SendOnLiveEnd             bool `json:"sendOnLiveEnd"`
	MessageCooldownSeconds    int  `json:"messageCooldownSeconds"`
	SendExpirationReminder    bool `json:"sendExpirationReminder"`
	ExpirationReminderMinutes int  `json:"expirationReminderMinutes"`
}

// ============================================
// Address Types
// ============================================

type AddressDTO struct {
	Street        string `json:"street"`
	Number        string `json:"number"`
	Complement    string `json:"complement"`
	District      string `json:"district"`
	City          string `json:"city"`
	State         string `json:"state"`
	Zip           string `json:"zip"`
	Country       string `json:"country"`
	StateRegister string `json:"stateRegister"`
}

// ============================================
// Shipping Defaults
// ============================================

type ShippingDefaultsDTO struct {
	PackageWeightGrams int    `json:"packageWeightGrams"`
	PackageFormat      string `json:"packageFormat"`
	// Optional default dimensions (cm). Used as a fallback when an
	// ERP-imported product (e.g. Tiny) only carries weight. Set ALL three
	// to enable the fallback; leave any nil to disable.
	HeightCm *int `json:"heightCm,omitempty"`
	WidthCm  *int `json:"widthCm,omitempty"`
	LengthCm *int `json:"lengthCm,omitempty"`
}

// ============================================
// Handler layer - Request types (ozzo Validate + ToInput)
// ============================================

type CreateStoreRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Validate is the syntactic gate (ozzo).
func (r CreateStoreRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(2, 100)),
		validation.Field(&r.Slug, validation.Required, validation.Length(2, 50), appvalidation.Slug),
	)
}

// ToInput builds the create usecase input. ClerkUserID comes from the JWT.
func (r CreateStoreRequest) ToInput(clerkUserID string) CreateStoreInput {
	return CreateStoreInput{
		Name:        r.Name,
		Slug:        r.Slug,
		ClerkUserID: clerkUserID,
	}
}

type UpdateStoreRequest struct {
	Name           string     `json:"name"`
	WhatsappNumber string     `json:"whatsappNumber"`
	EmailAddress   string     `json:"emailAddress"`
	SMSNumber      string     `json:"smsNumber"`
	Description    string     `json:"description"`
	Website        string     `json:"website"`
	LogoURL        string     `json:"logoUrl"`
	Address        AddressDTO `json:"address"`
	CNPJ           string     `json:"cnpj"`
}

// Validate is the syntactic gate (ozzo).
func (r UpdateStoreRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required, validation.Length(2, 100)),
	)
}

// ToInput builds the update usecase input for a given store. The service loads
// the current store, merges (subset semantics) and normalizes CEP/UF/CNPJ.
func (r UpdateStoreRequest) ToInput(storeID string) UpdateStoreInput {
	return UpdateStoreInput{
		StoreID:        storeID,
		Name:           r.Name,
		WhatsappNumber: r.WhatsappNumber,
		EmailAddress:   r.EmailAddress,
		SMSNumber:      r.SMSNumber,
		Description:    r.Description,
		Website:        r.Website,
		LogoURL:        r.LogoURL,
		Address:        r.Address,
		CNPJ:           r.CNPJ,
	}
}

type UpdateShippingDefaultsRequest struct {
	PackageWeightGrams int    `json:"packageWeightGrams"`
	PackageFormat      string `json:"packageFormat"`
	// Optional default dimensions (cm). All three must be provided together to
	// enable the ERP-import fallback; any nil disables it.
	HeightCm *int `json:"heightCm,omitempty"`
	WidthCm  *int `json:"widthCm,omitempty"`
	LengthCm *int `json:"lengthCm,omitempty"`
}

// Validate is the syntactic gate (ozzo).
func (r UpdateShippingDefaultsRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.PackageWeightGrams, validation.Min(0)),
		validation.Field(&r.PackageFormat, validation.Required, validation.In("box", "roll", "letter")),
		validation.Field(&r.HeightCm, validation.Min(1)),
		validation.Field(&r.WidthCm, validation.Min(1)),
		validation.Field(&r.LengthCm, validation.Min(1)),
	)
}

// ToInput builds the shipping-defaults usecase input for a given store.
func (r UpdateShippingDefaultsRequest) ToInput(storeID string) UpdateShippingDefaultsInput {
	return UpdateShippingDefaultsInput{
		StoreID:            storeID,
		PackageWeightGrams: r.PackageWeightGrams,
		PackageFormat:      r.PackageFormat,
		HeightCm:           r.HeightCm,
		WidthCm:            r.WidthCm,
		LengthCm:           r.LengthCm,
	}
}

type UpdateCartSettingsRequest struct {
	Enabled             bool     `json:"enabled"`
	ExpirationMinutes   int      `json:"expirationMinutes"`
	ReserveStock        bool     `json:"reserveStock"`
	AllowStorePickup    bool     `json:"allowStorePickup"`
	MaxQuantityPerItem  int      `json:"maxQuantityPerItem"`
	AllowEdit           bool     `json:"allowEdit"`
	CheckoutSendMethods []string `json:"checkoutSendMethods"`
	// Automatic message settings
	RealTimeCart              bool `json:"realTimeCart"`
	SendOnLiveEnd             bool `json:"sendOnLiveEnd"`
	MessageCooldownSeconds    int  `json:"messageCooldownSeconds"`
	SendExpirationReminder    bool `json:"sendExpirationReminder"`
	ExpirationReminderMinutes int  `json:"expirationReminderMinutes"`
}

// Validate is the syntactic gate (ozzo).
//
// NOTE on Required + Min: ozzo skips every rule (except Required) for a zero
// value, so `Min(n)` alone lets an omitted/zero int slip past the floor — a
// PUT that drops expirationMinutes would persist 0, undercutting the business
// minimum. Pairing Required with Min closes that: Required treats the value
// int's 0 as empty and rejects it, so the floor actually holds.
func (r UpdateCartSettingsRequest) Validate() error {
	return validation.ValidateStruct(&r,
		// Piso 15: o CHECK stores_cart_expiration_minutes_check (000104) rejeita
		// abaixo disso. Sem alinhar aqui, um 5 passaria a validação e estouraria
		// no banco como 500 em vez de 422 com a mensagem de campo.
		validation.Field(&r.ExpirationMinutes, validation.Required, validation.Min(15), validation.Max(1440)),
		validation.Field(&r.MaxQuantityPerItem, validation.Required, validation.Min(1)),
		validation.Field(&r.MessageCooldownSeconds, validation.Min(0), validation.Max(300)),
		// Only meaningful when the reminder toggle is on; required only then.
		validation.Field(&r.ExpirationReminderMinutes,
			validation.When(r.SendExpirationReminder, validation.Required, validation.Min(1), validation.Max(60))),
	)
}

// ToInput builds the cart-settings usecase input for a given store.
func (r UpdateCartSettingsRequest) ToInput(storeID string) UpdateCartSettingsInput {
	return UpdateCartSettingsInput{
		StoreID:                   storeID,
		Enabled:                   r.Enabled,
		ExpirationMinutes:         r.ExpirationMinutes,
		ReserveStock:              r.ReserveStock,
		AllowStorePickup:          r.AllowStorePickup,
		MaxQuantityPerItem:        r.MaxQuantityPerItem,
		AllowEdit:                 r.AllowEdit,
		CheckoutSendMethods:       r.CheckoutSendMethods,
		RealTimeCart:              r.RealTimeCart,
		SendOnLiveEnd:             r.SendOnLiveEnd,
		MessageCooldownSeconds:    r.MessageCooldownSeconds,
		SendExpirationReminder:    r.SendExpirationReminder,
		ExpirationReminderMinutes: r.ExpirationReminderMinutes,
	}
}

// ============================================
// Handler layer - Response types (mappers from *domain.Store)
// ============================================

type CreateStoreResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
}

// NewCreateStoreResponse maps the freshly created Store entity to the create
// response.
func NewCreateStoreResponse(s *storedomain.Store) CreateStoreResponse {
	return CreateStoreResponse{
		ID:        s.ID().String(),
		Name:      s.Name(),
		Slug:      s.Slug().String(),
		CreatedAt: s.CreatedAt(),
	}
}

type StoreResponse struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Slug             string              `json:"slug"`
	Active           bool                `json:"active"`
	WhatsappNumber   string              `json:"whatsappNumber"`
	EmailAddress     string              `json:"emailAddress"`
	SMSNumber        string              `json:"smsNumber"`
	Description      string              `json:"description"`
	Website          string              `json:"website"`
	LogoURL          *string             `json:"logoUrl"`
	Address          AddressDTO          `json:"address"`
	CNPJ             string              `json:"cnpj"`
	CartSettings     CartSettingsDTO     `json:"cartSettings"`
	ShippingDefaults ShippingDefaultsDTO `json:"shippingDefaults"`
	CreatedAt        time.Time           `json:"createdAt"`
}

// NewStoreResponse maps a domain Store entity to its API response. This is the
// controller's outbound mapper: presentation knows the domain; the domain never
// knows the response. LogoURL stays nullable — the handler swaps the stored S3
// key for a presigned URL (via Store.SetLogoURL) before calling this.
func NewStoreResponse(s *storedomain.Store) StoreResponse {
	addr := s.Address()
	cart := s.CartSettings()
	ship := s.ShippingDefaults()

	return StoreResponse{
		ID:             s.ID().String(),
		Name:           s.Name(),
		Slug:           s.Slug().String(),
		Active:         s.Active(),
		WhatsappNumber: deref(s.WhatsappNumber()),
		EmailAddress:   deref(s.EmailAddress()),
		SMSNumber:      deref(s.SMSNumber()),
		Description:    deref(s.Description()),
		Website:        deref(s.Website()),
		LogoURL:        s.LogoURL(),
		Address: AddressDTO{
			Street:        addr.Street(),
			Number:        addr.Number(),
			Complement:    addr.Complement(),
			District:      addr.District(),
			City:          addr.City(),
			State:         addr.State(),
			Zip:           addr.Zip(),
			Country:       addr.Country(),
			StateRegister: addr.StateRegister(),
		},
		CNPJ: deref(s.CNPJ()),
		CartSettings: CartSettingsDTO{
			Enabled:                   cart.Enabled(),
			ExpirationMinutes:         cart.ExpirationMinutes(),
			ReserveStock:              cart.ReserveStock(),
			AllowStorePickup:          cart.AllowStorePickup(),
			MaxQuantityPerItem:        cart.MaxQuantityPerItem(),
			AllowEdit:                 cart.AllowEdit(),
			CheckoutSendMethods:       cart.CheckoutSendMethods(),
			RealTimeCart:              cart.RealTimeCart(),
			SendOnLiveEnd:             cart.SendOnLiveEnd(),
			MessageCooldownSeconds:    cart.MessageCooldownSeconds(),
			SendExpirationReminder:    cart.SendExpirationReminder(),
			ExpirationReminderMinutes: cart.ExpirationReminderMinutes(),
		},
		ShippingDefaults: ShippingDefaultsDTO{
			PackageWeightGrams: ship.PackageWeightGrams(),
			PackageFormat:      ship.PackageFormat(),
			HeightCm:           ship.HeightCm(),
			WidthCm:            ship.WidthCm(),
			LengthCm:           ship.LengthCm(),
		},
		CreatedAt: s.CreatedAt(),
	}
}

type UploadLogoResponse struct {
	LogoURL string        `json:"logoUrl"`
	Store   StoreResponse `json:"store"`
}

// ============================================
// Service layer - Input types (usecase returns *domain.Store)
// ============================================

type CreateStoreInput struct {
	Name        string
	Slug        string
	ClerkUserID string // Clerk user ID from JWT
}

type UpdateStoreInput struct {
	StoreID        string
	Name           string
	WhatsappNumber string
	EmailAddress   string
	SMSNumber      string
	Description    string
	Website        string
	LogoURL        string
	Address        AddressDTO
	CNPJ           string
}

type UpdateShippingDefaultsInput struct {
	StoreID            string
	PackageWeightGrams int
	PackageFormat      string
	HeightCm           *int
	WidthCm            *int
	LengthCm           *int
}

type UpdateCartSettingsInput struct {
	StoreID             string
	Enabled             bool
	ExpirationMinutes   int
	ReserveStock        bool
	AllowStorePickup    bool
	MaxQuantityPerItem  int
	AllowEdit           bool
	CheckoutSendMethods []string
	// Automatic message settings
	RealTimeCart              bool
	SendOnLiveEnd             bool
	MessageCooldownSeconds    int
	SendExpirationReminder    bool
	ExpirationReminderMinutes int
}

// ============================================
// Repository layer - persistence params
// ============================================

type CreateStoreParams struct {
	Name string
	Slug string
}

type UpdateStoreParams struct {
	ID                   string
	Name                 string
	WhatsappNumber       string
	EmailAddress         string
	SMSNumber            string
	Description          string
	Website              string
	LogoURL              string
	AddressStreet        string
	AddressNumber        string
	AddressComplement    string
	AddressDistrict      string
	AddressCity          string
	AddressState         string
	AddressZip           string
	AddressCountry       string
	AddressStateRegister string
	CNPJ                 string
}

type UpdateShippingDefaultsParams struct {
	ID                 string
	PackageWeightGrams int
	PackageFormat      string
	HeightCm           *int
	WidthCm            *int
	LengthCm           *int
}

type UpdateCartSettingsParams struct {
	ID                  string
	Enabled             bool
	ExpirationMinutes   int
	ReserveStock        bool
	AllowStorePickup    bool
	MaxQuantityPerItem  int
	AllowEdit           bool
	CheckoutSendMethods []string
	// Automatic message settings
	RealTimeCart              bool
	SendOnLiveEnd             bool
	MessageCooldownSeconds    int
	SendExpirationReminder    bool
	ExpirationReminderMinutes int
}

// deref is used by NewStoreResponse to flatten nullable string fields into the
// stable empty-string shape the frontend expects.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

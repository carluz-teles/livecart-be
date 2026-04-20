package store

import "time"

// ============================================
// Cart Settings Types
// ============================================

type CartSettingsDTO struct {
	Enabled            bool     `json:"enabled"`
	ExpirationMinutes  int      `json:"expirationMinutes"`
	ReserveStock       bool     `json:"reserveStock"`
	MaxItems           int      `json:"maxItems"`
	MaxQuantityPerItem int      `json:"maxQuantityPerItem"`
	AllowEdit          bool     `json:"allowEdit"`
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
	Street  string `json:"street"`
	City    string `json:"city"`
	State   string `json:"state"`
	Zip     string `json:"zip"`
	Country string `json:"country"`
}

// ============================================
// Handler layer
// ============================================

type CreateStoreRequest struct {
	Name string `json:"name" validate:"required,min=2,max=100"`
	Slug string `json:"slug" validate:"required,min=2,max=50,alphanum"`
}

type CreateStoreResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateStoreRequest struct {
	Name           string     `json:"name" validate:"required,min=2,max=100"`
	WhatsappNumber string     `json:"whatsappNumber"`
	EmailAddress   string     `json:"emailAddress"`
	SMSNumber      string     `json:"smsNumber"`
	Description    string     `json:"description"`
	Website        string     `json:"website"`
	LogoURL        string     `json:"logoUrl"`
	Address        AddressDTO `json:"address"`
	CNPJ           string     `json:"cnpj"`
}

type UpdateCartSettingsRequest struct {
	Enabled            bool     `json:"enabled"`
	ExpirationMinutes  int      `json:"expirationMinutes" validate:"gte=5,lte=1440"`
	ReserveStock       bool     `json:"reserveStock"`
	MaxItems           int      `json:"maxItems" validate:"gte=0"`
	MaxQuantityPerItem int      `json:"maxQuantityPerItem" validate:"gte=1"`
	AllowEdit          bool     `json:"allowEdit"`
	CheckoutSendMethods []string `json:"checkoutSendMethods"`
	// Automatic message settings
	RealTimeCart              bool `json:"realTimeCart"`
	SendOnLiveEnd             bool `json:"sendOnLiveEnd"`
	MessageCooldownSeconds    int  `json:"messageCooldownSeconds" validate:"gte=0,lte=300"`
	SendExpirationReminder    bool `json:"sendExpirationReminder"`
	ExpirationReminderMinutes int  `json:"expirationReminderMinutes" validate:"gte=1,lte=60"`
}

type StoreResponse struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Slug           string          `json:"slug"`
	Active         bool            `json:"active"`
	WhatsappNumber *string         `json:"whatsappNumber"`
	EmailAddress   *string         `json:"emailAddress"`
	SMSNumber      *string         `json:"smsNumber"`
	Description    *string         `json:"description"`
	Website        *string         `json:"website"`
	LogoURL        *string         `json:"logoUrl"`
	Address        *AddressDTO     `json:"address"`
	CNPJ           *string         `json:"cnpj"`
	CartSettings   CartSettingsDTO `json:"cartSettings"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type UploadLogoResponse struct {
	LogoURL string        `json:"logoUrl"`
	Store   StoreResponse `json:"store"`
}

// ============================================
// Service layer
// ============================================

type CreateStoreInput struct {
	Name        string
	Slug        string
	ClerkUserID string // Clerk user ID from JWT
}

type CreateStoreOutput struct {
	ID           string
	Name         string
	Slug         string
	MembershipID string
	CreatedAt    time.Time
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

type UpdateCartSettingsInput struct {
	StoreID            string
	Enabled            bool
	ExpirationMinutes  int
	ReserveStock       bool
	MaxItems           int
	MaxQuantityPerItem int
	AllowEdit          bool
	CheckoutSendMethods []string
	// Automatic message settings
	RealTimeCart              bool
	SendOnLiveEnd             bool
	MessageCooldownSeconds    int
	SendExpirationReminder    bool
	ExpirationReminderMinutes int
}

type StoreOutput struct {
	ID             string
	Name           string
	Slug           string
	Active         bool
	WhatsappNumber *string
	EmailAddress   *string
	SMSNumber      *string
	Description    *string
	Website        *string
	LogoURL        *string
	Address        *AddressDTO
	CNPJ           *string
	CartSettings   CartSettingsDTO
	CreatedAt      time.Time
}

// ============================================
// Repository layer
// ============================================

type CreateStoreParams struct {
	Name string
	Slug string
}

type UpdateStoreParams struct {
	ID             string
	Name           string
	WhatsappNumber string
	EmailAddress   string
	SMSNumber      string
	Description    string
	Website        string
	LogoURL        string
	AddressStreet  string
	AddressCity    string
	AddressState   string
	AddressZip     string
	AddressCountry string
	CNPJ           string
}

type UpdateCartSettingsParams struct {
	ID                 string
	Enabled            bool
	ExpirationMinutes  int
	ReserveStock       bool
	MaxItems           int
	MaxQuantityPerItem int
	AllowEdit          bool
	CheckoutSendMethods []string
	// Automatic message settings
	RealTimeCart              bool
	SendOnLiveEnd             bool
	MessageCooldownSeconds    int
	SendExpirationReminder    bool
	ExpirationReminderMinutes int
}

type StoreRow struct {
	ID             string
	Name           string
	Slug           string
	Active         bool
	WhatsappNumber *string
	EmailAddress   *string
	SMSNumber      *string
	Description    *string
	Website        *string
	LogoURL        *string
	AddressStreet  *string
	AddressCity    *string
	AddressState   *string
	AddressZip     *string
	AddressCountry *string
	CNPJ           *string
	CartSettings   CartSettingsDTO
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

package providers

import (
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/ratelimit"
)

// Factory creates provider instances based on configuration.
type Factory struct {
	logger  *zap.Logger
	logFunc LogFunc

	// OAuth app credentials for providers that need them
	mercadoPagoAppID     string
	mercadoPagoAppSecret string

	// Rate limit manager
	rateLimitManager *ratelimit.Manager

	// Provider constructors (injected to avoid import cycles)
	mercadoPagoConstructor MercadoPagoConstructor
	pagarmeConstructor     PagarmeConstructor
	tinyConstructor        TinyConstructor
	instagramConstructor   InstagramConstructor
}

// FactoryConfig contains configuration for the provider factory.
type FactoryConfig struct {
	Logger               *zap.Logger
	LogFunc              LogFunc
	MercadoPagoAppID     string
	MercadoPagoAppSecret string
	RateLimitManager     *ratelimit.Manager

	// Constructors - these should be injected from the payment/erp/social packages
	MercadoPagoConstructor MercadoPagoConstructor
	PagarmeConstructor     PagarmeConstructor
	TinyConstructor        TinyConstructor
	InstagramConstructor   InstagramConstructor
}

// NewFactory creates a new provider factory.
func NewFactory(cfg FactoryConfig) *Factory {
	return &Factory{
		logger:                 cfg.Logger,
		logFunc:                cfg.LogFunc,
		mercadoPagoAppID:       cfg.MercadoPagoAppID,
		mercadoPagoAppSecret:   cfg.MercadoPagoAppSecret,
		rateLimitManager:       cfg.RateLimitManager,
		mercadoPagoConstructor: cfg.MercadoPagoConstructor,
		pagarmeConstructor:     cfg.PagarmeConstructor,
		tinyConstructor:        cfg.TinyConstructor,
		instagramConstructor:   cfg.InstagramConstructor,
	}
}

// ProviderConfig contains all data needed to instantiate a provider.
type ProviderConfig struct {
	IntegrationID string
	StoreID       string
	Type          ProviderType
	Name          ProviderName
	Credentials   *Credentials
	Metadata      map[string]any
}

// CreateProvider creates a provider instance based on the configuration.
func (f *Factory) CreateProvider(cfg ProviderConfig) (Provider, error) {
	switch cfg.Type {
	case ProviderTypePayment:
		return f.createPaymentProvider(cfg)
	case ProviderTypeERP:
		return f.createERPProvider(cfg)
	case ProviderTypeSocial:
		return f.createSocialProvider(cfg)
	default:
		return nil, fmt.Errorf("unknown provider type: %s", cfg.Type)
	}
}

// CreatePaymentProvider creates and returns a PaymentProvider.
func (f *Factory) CreatePaymentProvider(cfg ProviderConfig) (PaymentProvider, error) {
	if cfg.Type != ProviderTypePayment {
		return nil, fmt.Errorf("provider type must be 'payment', got '%s'", cfg.Type)
	}
	return f.createPaymentProvider(cfg)
}

// CreateERPProvider creates and returns an ERPProvider.
func (f *Factory) CreateERPProvider(cfg ProviderConfig) (ERPProvider, error) {
	if cfg.Type != ProviderTypeERP {
		return nil, fmt.Errorf("provider type must be 'erp', got '%s'", cfg.Type)
	}
	return f.createERPProvider(cfg)
}

func (f *Factory) createPaymentProvider(cfg ProviderConfig) (PaymentProvider, error) {
	var limiter ratelimit.RateLimiter
	if f.rateLimitManager != nil {
		limiter = f.rateLimitManager.GetOrCreate(cfg.IntegrationID)
	}

	switch cfg.Name {
	case ProviderMercadoPago:
		if f.mercadoPagoConstructor == nil {
			return nil, fmt.Errorf("mercado_pago constructor not configured")
		}
		return f.mercadoPagoConstructor(MercadoPagoConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			AppID:         f.mercadoPagoAppID,
			AppSecret:     f.mercadoPagoAppSecret,
			Logger:        f.logger,
			LogFunc:       f.logFunc,
			RateLimiter:   limiter,
		})
	case ProviderPagarme:
		if f.pagarmeConstructor == nil {
			return nil, fmt.Errorf("pagarme constructor not configured")
		}
		return f.pagarmeConstructor(PagarmeConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			Logger:        f.logger,
			LogFunc:       f.logFunc,
			RateLimiter:   limiter,
		})
	default:
		return nil, fmt.Errorf("unknown payment provider: %s", cfg.Name)
	}
}

func (f *Factory) createERPProvider(cfg ProviderConfig) (ERPProvider, error) {
	var limiter ratelimit.RateLimiter
	if f.rateLimitManager != nil {
		limiter = f.rateLimitManager.GetOrCreate(cfg.IntegrationID)
	}

	switch cfg.Name {
	case ProviderTiny:
		if f.tinyConstructor == nil {
			return nil, fmt.Errorf("tiny constructor not configured")
		}
		// Get client credentials from the stored credentials (each customer has their own)
		var clientID, clientSecret string
		if cfg.Credentials != nil && cfg.Credentials.Extra != nil {
			if id, ok := cfg.Credentials.Extra["client_id"].(string); ok {
				clientID = id
			}
			if secret, ok := cfg.Credentials.Extra["client_secret"].(string); ok {
				clientSecret = secret
			}
		}
		return f.tinyConstructor(TinyConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			ClientID:      clientID,
			ClientSecret:  clientSecret,
			Logger:        f.logger,
			LogFunc:       f.logFunc,
			RateLimiter:   limiter,
		})
	default:
		return nil, fmt.Errorf("unknown ERP provider: %s", cfg.Name)
	}
}

// =============================================================================
// PROVIDER CONSTRUCTOR TYPES
// =============================================================================

// MercadoPagoConstructor is a function type for creating Mercado Pago providers.
type MercadoPagoConstructor func(cfg MercadoPagoConfig) (PaymentProvider, error)

// PagarmeConstructor is a function type for creating Pagar.me providers.
type PagarmeConstructor func(cfg PagarmeConfig) (PaymentProvider, error)

// TinyConstructor is a function type for creating Tiny providers.
type TinyConstructor func(cfg TinyConfig) (ERPProvider, error)

// MercadoPagoConfig contains configuration for Mercado Pago provider.
type MercadoPagoConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	AppID         string
	AppSecret     string
	Logger        *zap.Logger
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// PagarmeConfig contains configuration for Pagar.me provider.
type PagarmeConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	Logger        *zap.Logger
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// TinyConfig contains configuration for Tiny ERP provider.
type TinyConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	ClientID      string
	ClientSecret  string
	Logger        *zap.Logger
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// InstagramConstructor is a function type for creating Instagram providers.
type InstagramConstructor func(cfg InstagramConfig) (SocialProvider, error)

// InstagramConfig contains configuration for Instagram provider.
type InstagramConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	Logger        *zap.Logger
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// CreateSocialProvider creates and returns a SocialProvider.
func (f *Factory) CreateSocialProvider(cfg ProviderConfig) (SocialProvider, error) {
	if cfg.Type != ProviderTypeSocial {
		return nil, fmt.Errorf("provider type must be 'social', got '%s'", cfg.Type)
	}
	return f.createSocialProvider(cfg)
}

func (f *Factory) createSocialProvider(cfg ProviderConfig) (SocialProvider, error) {
	var limiter ratelimit.RateLimiter
	if f.rateLimitManager != nil {
		limiter = f.rateLimitManager.GetOrCreate(cfg.IntegrationID)
	}

	switch cfg.Name {
	case ProviderInstagram:
		if f.instagramConstructor == nil {
			return nil, fmt.Errorf("instagram constructor not configured")
		}
		return f.instagramConstructor(InstagramConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			Logger:        f.logger,
			LogFunc:       f.logFunc,
			RateLimiter:   limiter,
		})
	default:
		return nil, fmt.Errorf("unknown social provider: %s", cfg.Name)
	}
}

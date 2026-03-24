package providers

import (
	"fmt"

	"go.uber.org/zap"
)

// Factory creates provider instances based on configuration.
type Factory struct {
	logger  *zap.Logger
	logFunc LogFunc

	// OAuth app credentials for providers that need them
	mercadoPagoAppID     string
	mercadoPagoAppSecret string

	// Provider constructors (injected to avoid import cycles)
	mercadoPagoConstructor MercadoPagoConstructor
	tinyConstructor        TinyConstructor
}

// FactoryConfig contains configuration for the provider factory.
type FactoryConfig struct {
	Logger               *zap.Logger
	LogFunc              LogFunc
	MercadoPagoAppID     string
	MercadoPagoAppSecret string

	// Constructors - these should be injected from the payment/erp packages
	MercadoPagoConstructor MercadoPagoConstructor
	TinyConstructor        TinyConstructor
}

// NewFactory creates a new provider factory.
func NewFactory(cfg FactoryConfig) *Factory {
	return &Factory{
		logger:                 cfg.Logger,
		logFunc:                cfg.LogFunc,
		mercadoPagoAppID:       cfg.MercadoPagoAppID,
		mercadoPagoAppSecret:   cfg.MercadoPagoAppSecret,
		mercadoPagoConstructor: cfg.MercadoPagoConstructor,
		tinyConstructor:        cfg.TinyConstructor,
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
		})
	default:
		return nil, fmt.Errorf("unknown payment provider: %s", cfg.Name)
	}
}

func (f *Factory) createERPProvider(cfg ProviderConfig) (ERPProvider, error) {
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
}

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

	// Melhor Envio OAuth app credentials (one app per environment)
	melhorEnvioClientID     string
	melhorEnvioClientSecret string
	melhorEnvioEnv          string // "sandbox" or "production"
	melhorEnvioUserAgent    string
	melhorEnvioRedirectURI  string

	// Rate limit manager
	rateLimitManager *ratelimit.Manager

	// SmartEnvios configuration (token-based — no OAuth).
	smartEnviosEnv       string // "sandbox" or "production"
	smartEnviosUserAgent string

	// Twilio master account (WhatsApp — per-store subaccounts live in the
	// integration credentials; the master signs webhooks and creates
	// subaccounts).
	twilioAccountSID string
	twilioAuthToken  string

	// Provider constructors (injected to avoid import cycles)
	mercadoPagoConstructor MercadoPagoConstructor
	pagarmeConstructor     PagarmeConstructor
	tinyConstructor        TinyConstructor
	instagramConstructor   InstagramConstructor
	melhorEnvioConstructor MelhorEnvioConstructor
	smartEnviosConstructor SmartEnviosConstructor
	twilioConstructor      TwilioConstructor
}

// FactoryConfig contains configuration for the provider factory.
type FactoryConfig struct {
	Logger               *zap.Logger
	LogFunc              LogFunc
	MercadoPagoAppID     string
	MercadoPagoAppSecret string
	RateLimitManager     *ratelimit.Manager

	// Melhor Envio OAuth app (each env has its own app/credentials)
	MelhorEnvioClientID     string
	MelhorEnvioClientSecret string
	MelhorEnvioEnv          string // "sandbox" or "production"
	MelhorEnvioUserAgent    string
	MelhorEnvioRedirectURI  string

	// SmartEnvios (token-based — no OAuth).
	SmartEnviosEnv       string // "sandbox" or "production"
	SmartEnviosUserAgent string

	// Twilio master account (WhatsApp)
	TwilioAccountSID string
	TwilioAuthToken  string

	// Constructors - these should be injected from the payment/erp/social/shipping packages
	MercadoPagoConstructor MercadoPagoConstructor
	PagarmeConstructor     PagarmeConstructor
	TinyConstructor        TinyConstructor
	InstagramConstructor   InstagramConstructor
	MelhorEnvioConstructor MelhorEnvioConstructor
	SmartEnviosConstructor SmartEnviosConstructor
	TwilioConstructor      TwilioConstructor
}

// NewFactory creates a new provider factory.
func NewFactory(cfg FactoryConfig) *Factory {
	return &Factory{
		logger:                  cfg.Logger,
		logFunc:                 cfg.LogFunc,
		mercadoPagoAppID:        cfg.MercadoPagoAppID,
		mercadoPagoAppSecret:    cfg.MercadoPagoAppSecret,
		melhorEnvioClientID:     cfg.MelhorEnvioClientID,
		melhorEnvioClientSecret: cfg.MelhorEnvioClientSecret,
		melhorEnvioEnv:          cfg.MelhorEnvioEnv,
		melhorEnvioUserAgent:    cfg.MelhorEnvioUserAgent,
		melhorEnvioRedirectURI:  cfg.MelhorEnvioRedirectURI,
		smartEnviosEnv:          cfg.SmartEnviosEnv,
		smartEnviosUserAgent:    cfg.SmartEnviosUserAgent,
		twilioAccountSID:        cfg.TwilioAccountSID,
		twilioAuthToken:         cfg.TwilioAuthToken,
		rateLimitManager:        cfg.RateLimitManager,
		mercadoPagoConstructor:  cfg.MercadoPagoConstructor,
		pagarmeConstructor:      cfg.PagarmeConstructor,
		tinyConstructor:         cfg.TinyConstructor,
		instagramConstructor:    cfg.InstagramConstructor,
		melhorEnvioConstructor:  cfg.MelhorEnvioConstructor,
		smartEnviosConstructor:  cfg.SmartEnviosConstructor,
		twilioConstructor:       cfg.TwilioConstructor,
	}
}

// ProviderConfig contains all data needed to instantiate a provider.
// MetadataUseAvailableStock é a chave, no metadata da integração, que liga o
// espelhamento do saldo VENDÁVEL do ERP em vez do físico.
//
// Vive no metadata e não numa coluna porque é configuração de UMA integração,
// não da loja: um lojista pode ter mais de um ERP ligado, e a diferença entre
// físico e disponível é um conceito do Tiny.
const MetadataUseAvailableStock = "use_available_stock"

// MetadataBool lê uma chave booleana do metadata da integração.
//
// Ausente, nulo ou de outro tipo devolve false — o padrão de toda configuração
// nova é ficar desligada, e um metadata malformado não pode ligar sozinho um
// comportamento que muda o que a loja vende.
func MetadataBool(metadata map[string]any, chave string) bool {
	if metadata == nil {
		return false
	}
	v, ok := metadata[chave].(bool)
	return ok && v
}

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
	case ProviderTypeShipping:
		return f.createShippingProvider(cfg)
	case ProviderTypeCommunication:
		return f.createCommunicationProvider(cfg)
	default:
		return nil, fmt.Errorf("unknown provider type: %s", cfg.Type)
	}
}

// CreateCommunicationProvider creates and returns a CommunicationProvider.
func (f *Factory) CreateCommunicationProvider(cfg ProviderConfig) (CommunicationProvider, error) {
	if cfg.Type != ProviderTypeCommunication {
		return nil, fmt.Errorf("provider type must be 'communication', got '%s'", cfg.Type)
	}
	return f.createCommunicationProvider(cfg)
}

func (f *Factory) createCommunicationProvider(cfg ProviderConfig) (CommunicationProvider, error) {
	var limiter ratelimit.RateLimiter
	if f.rateLimitManager != nil {
		limiter = f.rateLimitManager.GetOrCreate(cfg.IntegrationID)
	}

	switch cfg.Name {
	case ProviderTwilioWhatsApp:
		if f.twilioConstructor == nil {
			return nil, fmt.Errorf("twilio_whatsapp constructor not configured")
		}
		return f.twilioConstructor(TwilioConfig{
			IntegrationID:    cfg.IntegrationID,
			StoreID:          cfg.StoreID,
			Credentials:      cfg.Credentials,
			Metadata:         cfg.Metadata,
			MasterAccountSID: f.twilioAccountSID,
			MasterAuthToken:  f.twilioAuthToken,
			Logger:           f.logger,
			LogFunc:          f.logFunc,
			RateLimiter:      limiter,
		})
	default:
		return nil, fmt.Errorf("unknown communication provider: %s", cfg.Name)
	}
}

// TwilioConfig is the provider-agnostic config handed to the Twilio
// constructor (defined here to avoid import cycles, like the others).
type TwilioConfig struct {
	IntegrationID    string
	StoreID          string
	Credentials      *Credentials
	Metadata         map[string]any
	MasterAccountSID string
	MasterAuthToken  string
	Logger           *zap.Logger
	LogFunc          LogFunc
	RateLimiter      ratelimit.RateLimiter
}

// TwilioConstructor creates a Twilio WhatsApp provider (injected from the
// communication package).
type TwilioConstructor func(cfg TwilioConfig) (CommunicationProvider, error)

// CreateShippingProvider creates and returns a ShippingProvider.
func (f *Factory) CreateShippingProvider(cfg ProviderConfig) (ShippingProvider, error) {
	if cfg.Type != ProviderTypeShipping {
		return nil, fmt.Errorf("provider type must be 'shipping', got '%s'", cfg.Type)
	}
	return f.createShippingProvider(cfg)
}

func (f *Factory) createShippingProvider(cfg ProviderConfig) (ShippingProvider, error) {
	var limiter ratelimit.RateLimiter
	if f.rateLimitManager != nil {
		limiter = f.rateLimitManager.GetOrCreate(cfg.IntegrationID)
	}

	switch cfg.Name {
	case ProviderMelhorEnvio:
		if f.melhorEnvioConstructor == nil {
			return nil, fmt.Errorf("melhor_envio constructor not configured")
		}
		return f.melhorEnvioConstructor(MelhorEnvioConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			ClientID:      f.melhorEnvioClientID,
			ClientSecret:  f.melhorEnvioClientSecret,
			Env:           f.melhorEnvioEnv,
			UserAgent:     f.melhorEnvioUserAgent,
			RedirectURI:   f.melhorEnvioRedirectURI,
			Logger:        f.logger,
			LogFunc:       f.logFunc,
			RateLimiter:   limiter,
		})
	case ProviderSmartEnvios:
		if f.smartEnviosConstructor == nil {
			return nil, fmt.Errorf("smartenvios constructor not configured")
		}
		env := f.smartEnviosEnv
		if m, ok := cfg.Metadata["environment"].(string); ok && m != "" {
			env = m
		}
		return f.smartEnviosConstructor(SmartEnviosConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			Env:           env,
			UserAgent:     f.smartEnviosUserAgent,
			Logger:        f.logger,
			LogFunc:       f.logFunc,
			RateLimiter:   limiter,
		})
	default:
		return nil, fmt.Errorf("unknown shipping provider: %s", cfg.Name)
	}
}

// MelhorEnvioConstructor is a function type for creating Melhor Envio providers.
type MelhorEnvioConstructor func(cfg MelhorEnvioConfig) (ShippingProvider, error)

// MelhorEnvioConfig contains configuration for a Melhor Envio provider instance.
type MelhorEnvioConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	ClientID      string
	ClientSecret  string
	Env           string // "sandbox" or "production"
	UserAgent     string // "AppName (contact@email)" - required by the ME API
	RedirectURI   string
	Logger        *zap.Logger
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// SmartEnviosConstructor is a function type for creating SmartEnvios providers.
type SmartEnviosConstructor func(cfg SmartEnviosConfig) (ShippingProvider, error)

// SmartEnviosConfig contains configuration for a SmartEnvios provider instance.
// SmartEnvios uses a static token (no OAuth) — the token lives in Credentials.AccessToken.
type SmartEnviosConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	Env           string // "sandbox" or "production"
	UserAgent     string // optional — some carriers block generic UAs
	Logger        *zap.Logger
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
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
			IntegrationID:     cfg.IntegrationID,
			StoreID:           cfg.StoreID,
			Credentials:       cfg.Credentials,
			ClientID:          clientID,
			ClientSecret:      clientSecret,
			Logger:            f.logger,
			LogFunc:           f.logFunc,
			RateLimiter:       limiter,
			UseAvailableStock: MetadataBool(cfg.Metadata, MetadataUseAvailableStock),
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
	// UseAvailableStock espelha o saldo VENDÁVEL do Tiny em vez do físico.
	// Desligado por padrão — ver MetadataUseAvailableStock.
	UseAvailableStock bool
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

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

	// Credenciais do APLICATIVO Bling. Diferente do Tiny, onde cada lojista cria
	// o próprio aplicativo e cola client_id/secret: no Bling o LiveCart tem UM
	// aplicativo, e o mesmo client_secret assina o HMAC dos webhooks de TODAS as
	// lojas. Rotacioná-lo invalida o Basic do token endpoint E a assinatura de
	// todos os webhooks ao mesmo tempo.
	blingClientID     string
	blingClientSecret string

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
	blingConstructor       BlingConstructor
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
	BlingClientID        string
	BlingClientSecret    string
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
	BlingConstructor       BlingConstructor
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
		blingClientID:           cfg.BlingClientID,
		blingClientSecret:       cfg.BlingClientSecret,
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
		blingConstructor:        cfg.BlingConstructor,
		instagramConstructor:    cfg.InstagramConstructor,
		melhorEnvioConstructor:  cfg.MelhorEnvioConstructor,
		smartEnviosConstructor:  cfg.SmartEnviosConstructor,
		twilioConstructor:       cfg.TwilioConstructor,
	}
}

// ProviderConfig contains all data needed to instantiate a provider.
// MetadataResyncRunningSince marca, no metadata da integração, o instante em que
// a releitura em massa começou. Ausente = nenhuma em andamento.
//
// Persistido e não em memória porque a resposta precisa valer para QUALQUER pod:
// o lojista pode abrir o painel numa aba servida por outra instância, e um
// estado só na memória de quem enfileirou diria "parado" para ela.
const MetadataResyncRunningSince = "resync_running_since"

// MetadataResyncDone e MetadataResyncTotal levam o progresso da varredura.
//
// Escritos no mesmo metadata da marca de andamento porque são a mesma coisa
// vista de perto: "está rodando" é a existência da marca, "vai em 42 de 154" é
// o detalhe que faz dezessete minutos de espera parecerem trabalho em vez de
// travamento.
const (
	MetadataResyncDone  = "resync_done"
	MetadataResyncTotal = "resync_total"
)

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

// BlingRPSPadrao é o freio local para o Bling.
//
// O teto real é 3 req/s POR CONTA somando TODOS os apps do lojista — se ele tem
// e-commerce ou marketplace no mesmo Bling, eles comem da mesma cota e são
// invisíveis para nós, sem header nenhum para reconciliar. 2 e não 3 porque
// errar para menos custa latência e errar para mais custa a venda.
var BlingRPSPadrao = 2.0

func (f *Factory) createERPProvider(cfg ProviderConfig) (ERPProvider, error) {
	var limiter ratelimit.RateLimiter
	if f.rateLimitManager != nil {
		if cfg.Name == ProviderBling {
			// Duas decisões, ambas medidas:
			//
			// 1. Limitador PREDITIVO. A API do Bling não devolve header de cota
			//    (medido: 23 headers numa resposta 200, nenhum deles de cota), e
			//    o AdaptiveLimiter sem header devolve "pode passar" para sempre.
			// 2. Chave pela CONTA, não pela integração — o teto é por conta.
			//    Sem conta conhecida ainda (primeira conexão), cai no id da
			//    integração: um balde a mais é melhor do que balde nenhum.
			chave := chaveDeCotaBling(cfg)
			limiter = f.rateLimitManager.GetOrCreateFixo(chave, BlingRPSPadrao)
		} else {
			limiter = f.rateLimitManager.GetOrCreate(cfg.IntegrationID)
		}
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
	case ProviderBling:
		if f.blingConstructor == nil {
			return nil, fmt.Errorf("bling constructor not configured")
		}
		// As credenciais do aplicativo vêm do ambiente (app único do LiveCart),
		// com escape para o lojista que preferir o PRÓPRIO aplicativo privado —
		// mesmo padrão de fonte dupla que o Tiny já usa em Credentials.Extra.
		clientID, clientSecret := f.blingClientID, f.blingClientSecret
		if cfg.Credentials != nil && cfg.Credentials.Extra != nil {
			if v, ok := cfg.Credentials.Extra["client_id"].(string); ok && v != "" {
				clientID = v
			}
			if v, ok := cfg.Credentials.Extra["client_secret"].(string); ok && v != "" {
				clientSecret = v
			}
		}
		var contaID string
		if cfg.Metadata != nil {
			contaID, _ = cfg.Metadata[MetadataBlingCompanyID].(string)
		}
		return f.blingConstructor(BlingConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Credentials:   cfg.Credentials,
			ClientID:      clientID,
			ClientSecret:  clientSecret,
			ContaID:       contaID,
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

// chaveDeCotaBling devolve a chave do balde: a conta do ERP quando conhecida.
func chaveDeCotaBling(cfg ProviderConfig) string {
	if cfg.Metadata != nil {
		if id, _ := cfg.Metadata[MetadataBlingCompanyID].(string); id != "" {
			return "bling:conta:" + id
		}
	}
	return "bling:integracao:" + cfg.IntegrationID
}

// TinyConstructor is a function type for creating Tiny providers.
type TinyConstructor func(cfg TinyConfig) (ERPProvider, error)

// BlingConstructor cria o provider do Bling (injetado do pacote erp, como os outros).
type BlingConstructor func(cfg BlingConfig) (ERPProvider, error)

// BlingConfig é a configuração do provider do Bling.
type BlingConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	ClientID      string
	ClientSecret  string
	// ContaID é o `data.id` de GET /empresas/me/dados-basicos — a identidade da
	// CONTA, que é a chave de cota correta (o teto do Bling é POR CONTA, não por
	// integração) e o mesmo valor que o webhook manda como `companyId`.
	ContaID     string
	Logger      *zap.Logger
	LogFunc     LogFunc
	RateLimiter ratelimit.RateLimiter
}

// MetadataBlingCompanyID é a chave do metadata onde a identidade da conta Bling
// é guardada em espelho da coluna integrations.erp_account_id, para o factory
// não precisar de uma consulta a mais na construção do provider.
const MetadataBlingCompanyID = "bling_company_id"

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

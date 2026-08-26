package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Key represents an environment variable key
type Key string

// Environment keys
const (
	AppEnv             Key = "APP_ENV"
	LogLevel           Key = "LOG_LEVEL" // debug | info | warn | error (default do ambiente se vazio)
	Port               Key = "PORT"
	DatabaseURL        Key = "DATABASE_URL"
	ClerkFrontendAPI   Key = "CLERK_FRONTEND_API"
	ClerkWebhookSecret Key = "CLERK_WEBHOOK_SECRET"
	AWSRegion          Key = "AWS_REGION"
	AWSAccessKeyID     Key = "AWS_ACCESS_KEY_ID"
	AWSSecretAccessKey Key = "AWS_SECRET_ACCESS_KEY"

	// Async events / telemetry
	RedisURL  Key = "REDIS_URL" // Full redis[s]:// connection URL (Railway/managed Redis: carries user/password/TLS). Takes precedence over REDIS_ADDR.
	RedisAddr Key = "REDIS_ADDR"
	// ERPWritePipeline liga a fila serial por pedido e o limiter com os tetos
	// medidos da API do Tiny (4/s em rajada, 30/min por conta). Desligado por
	// padrão — o caminho legado continua sendo o default durante a migração.
	ERPWritePipeline     Key = "ERP_WRITE_PIPELINE"          // Redis host:port for the asynq event queue (local/no-auth; default localhost:6379)
	OTELExporterEndpoint Key = "OTEL_EXPORTER_OTLP_ENDPOINT" // OTLP gRPC endpoint (Jaeger local, Datadog agent in prod); empty = tracing no-op
	OTELServiceName      Key = "OTEL_SERVICE_NAME"           // service.name resource attr (default livecart-api)
	NewRelicLicenseKey   Key = "NEW_RELIC_LICENSE_KEY"       // Insights Event API ingest key; empty = New Relic custom-events exporter disabled
	NewRelicAccountID    Key = "NEW_RELIC_ACCOUNT_ID"        // New Relic account id the custom events are posted to

	// Integration Layer
	EncryptionKey        Key = "ENCRYPTION_KEY"          // Base64-encoded 32-byte key for AES-GCM
	WebhookBaseURL       Key = "WEBHOOK_BASE_URL"        // Base URL for webhook callbacks (e.g., https://api.livecart.com)
	FrontendURL          Key = "FRONTEND_URL"            // Frontend URL for redirects (e.g., https://livecart.com)
	MercadoPagoAppID     Key = "MERCADO_PAGO_APP_ID"     // Mercado Pago OAuth App ID
	MercadoPagoAppSecret Key = "MERCADO_PAGO_APP_SECRET" // Mercado Pago OAuth App Secret
	MercadoPagoTestMode  Key = "MERCADO_PAGO_TEST_MODE"  // Set to "true" to get TEST credentials via OAuth

	// PagarmeAntifraudDisabledStores is a comma-separated allowlist of store IDs
	// for which we send antifraud_enabled=false on Pagar.me card orders. Escape
	// hatch for accounts whose antifraud régua reproves legit sales as "high"
	// (acquirer approves, Pagar.me antifraud reproves) — scoped per store so we
	// don't strip fraud protection from merchants whose accounts are fine.
	// Flipping the list is a config/env change, not a code deploy. Chargeback
	// liability shifts to the merchant for the listed stores.
	PagarmeAntifraudDisabledStores Key = "PAGARME_ANTIFRAUD_DISABLED_STORES"

	// Instagram/Meta Integration
	InstagramVerifyToken Key = "INSTAGRAM_VERIFY_TOKEN" // Token for Meta webhook verification
	InstagramAppID       Key = "INSTAGRAM_APP_ID"       // Instagram OAuth App ID (from Meta for Developers)
	InstagramAppSecret   Key = "INSTAGRAM_APP_SECRET"   // Instagram OAuth App Secret
	// InstagramWebhookEnforceSignature gates the X-Hub-Signature-256 check on
	// POST /api/webhooks/instagram. Left false (deploy 1) the signature is
	// verified and logged but the payload is still processed, so a misconfigured
	// secret cannot silently stop every comment from becoming a cart. Flip to
	// true (deploy 2) only after the logs show no valid traffic being rejected.
	InstagramWebhookEnforceSignature Key = "INSTAGRAM_WEBHOOK_ENFORCE_SIGNATURE"

	// Melhor Envio (shipping provider)
	MelhorEnvioClientID     Key = "MELHOR_ENVIO_CLIENT_ID"     // OAuth App ID from Melhor Envio panel
	MelhorEnvioClientSecret Key = "MELHOR_ENVIO_CLIENT_SECRET" // OAuth App Secret from Melhor Envio panel
	MelhorEnvioEnv          Key = "MELHOR_ENVIO_ENV"           // "sandbox" or "production"
	MelhorEnvioUserAgent    Key = "MELHOR_ENVIO_USER_AGENT"    // Required header, format: "LiveCart (contato@livecart.com.br)"
	MelhorEnvioRedirectURI  Key = "MELHOR_ENVIO_REDIRECT_URI"  // OAuth callback URL configured in Melhor Envio app

	// SmartEnvios (shipping provider — static-token auth, no OAuth)
	SmartEnviosEnv       Key = "SMARTENVIOS_ENV"        // "sandbox" or "production" (default)
	SmartEnviosUserAgent Key = "SMARTENVIOS_USER_AGENT" // optional — sent on every SmartEnvios request

	// Twilio (WhatsApp communication provider — PRD 006)
	TwilioAccountSID Key = "TWILIO_ACCOUNT_SID" // Master account SID (Console -> Account Info)
	TwilioAuthToken  Key = "TWILIO_AUTH_TOKEN"  // Master auth token — also signs webhook validation

	// OpenRouter (LLM gateway — WhatsApp sales assistant, catalog RAG)
	OpenRouterAPIKey Key = "OPENROUTER_API_KEY" // Bearer key for https://openrouter.ai/api/v1
	OpenRouterModel  Key = "OPENROUTER_MODEL"   // model id (see NewOpenRouterClient default)

	// Stripe (paywall/assinaturas — PRD 007)
	StripeSecretKey     Key = "STRIPE_SECRET_KEY"     // sk_test_/sk_live_
	StripeWebhookSecret Key = "STRIPE_WEBHOOK_SECRET" // whsec_ do endpoint /api/webhooks/stripe

	StripePriceStartFlat    Key = "STRIPE_PRICE_START_FLAT"
	StripePriceStartMetered Key = "STRIPE_PRICE_START_METERED"
	StripePriceGrowFlat     Key = "STRIPE_PRICE_GROW_FLAT"
	StripePriceGrowMetered  Key = "STRIPE_PRICE_GROW_METERED"
	StripePriceScaleFlat    Key = "STRIPE_PRICE_SCALE_FLAT"
	StripePriceScaleMetered Key = "STRIPE_PRICE_SCALE_METERED"
	StripeGMVMeterEvent     Key = "STRIPE_GMV_METER_EVENT" // default: gmv_cents
	PaywallEnabled          Key = "PAYWALL_ENABLED"        // default false: trial/ledger/meter rodam, mas NADA bloqueia

	// S3 Storage (supports both standard and Railway naming conventions)
	S3Bucket   Key = "S3_BUCKET"    // S3 bucket name for uploads
	S3Endpoint Key = "S3_ENDPOINT"  // Custom S3 endpoint (for Tigris, R2, MinIO, etc.)
	CDNBaseURL Key = "CDN_BASE_URL" // Optional CDN base URL for uploaded files

	// Railway alternative names
	AWSS3BucketName  Key = "AWS_S3_BUCKET_NAME" // Railway: S3 bucket name
	AWSEndpointURL   Key = "AWS_ENDPOINT_URL"   // Railway: S3 endpoint
	AWSDefaultRegion Key = "AWS_DEFAULT_REGION" // Railway: AWS region
)

// Environment values
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
	EnvStaging     = "staging"
	EnvTest        = "test"
)

// Load loads environment variables from .env file
// It silently ignores if .env file doesn't exist
func Load() error {
	// Try to load .env file, ignore error if it doesn't exist
	_ = godotenv.Load()
	return nil
}

// LoadFrom loads environment variables from a specific file
func LoadFrom(filename string) error {
	return godotenv.Load(filename)
}

// String returns the value of the environment variable as string
func (k Key) String() string {
	return os.Getenv(string(k))
}

// StringOr returns the value of the environment variable or a default value
func (k Key) StringOr(defaultValue string) string {
	if v := k.String(); v != "" {
		return v
	}
	return defaultValue
}

// Int returns the value of the environment variable as int
func (k Key) Int() int {
	v, _ := strconv.Atoi(k.String())
	return v
}

// IntOr returns the value of the environment variable as int or a default value
func (k Key) IntOr(defaultValue int) int {
	if v := k.String(); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultValue
}

// Bool returns the value of the environment variable as bool
func (k Key) Bool() bool {
	v, _ := strconv.ParseBool(k.String())
	return v
}

// BoolOr returns the value of the environment variable as bool or a default value
func (k Key) BoolOr(defaultValue bool) bool {
	if v := k.String(); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return defaultValue
}

// Required returns the value of the environment variable or panics if empty
func (k Key) Required() string {
	v := k.String()
	if v == "" {
		panic("required environment variable not set: " + string(k))
	}
	return v
}

// IsSet returns true if the environment variable is set and not empty
func (k Key) IsSet() bool {
	return k.String() != ""
}

// Environment returns the current environment (development, production, staging, test)
func Environment() string {
	return AppEnv.StringOr(EnvDevelopment)
}

// IsDevelopment returns true if running in development environment
func IsDevelopment() bool {
	return Environment() == EnvDevelopment
}

// IsProduction returns true if running in production environment
func IsProduction() bool {
	return Environment() == EnvProduction
}

// IsStaging returns true if running in staging environment
func IsStaging() bool {
	return Environment() == EnvStaging
}

// IsTest returns true if running in test environment
func IsTest() bool {
	return Environment() == EnvTest
}

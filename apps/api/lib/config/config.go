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
	Port               Key = "PORT"
	DatabaseURL        Key = "DATABASE_URL"
	ClerkFrontendAPI   Key = "CLERK_FRONTEND_API"
	ClerkWebhookSecret Key = "CLERK_WEBHOOK_SECRET"
	AWSRegion          Key = "AWS_REGION"
	AWSAccessKeyID     Key = "AWS_ACCESS_KEY_ID"
	AWSSecretAccessKey Key = "AWS_SECRET_ACCESS_KEY"
	SQSQueueURL        Key = "SQS_QUEUE_URL"

	// Integration Layer
	EncryptionKey        Key = "ENCRYPTION_KEY"          // Base64-encoded 32-byte key for AES-GCM
	WebhookBaseURL       Key = "WEBHOOK_BASE_URL"        // Base URL for webhook callbacks (e.g., https://api.livecart.com)
	FrontendURL          Key = "FRONTEND_URL"            // Frontend URL for redirects (e.g., https://livecart.com)
	MercadoPagoAppID     Key = "MERCADO_PAGO_APP_ID"     // Mercado Pago OAuth App ID
	MercadoPagoAppSecret Key = "MERCADO_PAGO_APP_SECRET" // Mercado Pago OAuth App Secret
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

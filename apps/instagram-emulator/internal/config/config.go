package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds the emulator configuration
type Config struct {
	// Server settings
	Port int

	// Webhook settings
	WebhookURL  string
	VerifyToken string

	// Instagram account settings
	AccountID string
	Username  string

	// Live media ID (optional - if set, uses this instead of generating UUID)
	MediaID string
}

// Load loads configuration from environment variables
func Load() *Config {
	// Try to load .env file (ignore error if not found)
	_ = godotenv.Load()

	return &Config{
		Port:        getEnvInt("EMULATOR_PORT", 8080),
		WebhookURL:  getEnv("EMULATOR_WEBHOOK_URL", "http://localhost:3001/webhook/instagram"),
		VerifyToken: getEnv("EMULATOR_VERIFY_TOKEN", "livecart_verify_token"),
		AccountID:   getEnv("EMULATOR_ACCOUNT_ID", "17841405822304914"),
		Username:    getEnv("EMULATOR_USERNAME", "loja_livecart"),
		MediaID:     getEnv("EMULATOR_MEDIA_ID", ""),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

package exporter

// Config configures the New Relic custom-events exporter. It follows the same
// feature-flag pattern as internal/telemetry.Config (Endpoint == "" disables
// OTEL export): Enabled is derived from whether a license key was provided,
// not toggled independently.
type Config struct {
	// APIKey is the New Relic Insights Event API ingest key (NEW_RELIC_LICENSE_KEY).
	APIKey string
	// AccountID is the New Relic account id the events are posted to
	// (NEW_RELIC_ACCOUNT_ID).
	AccountID string
	// Enabled is false when APIKey is empty (local dev / tests without a New
	// Relic account configured). Every exporter call site must no-op when false.
	Enabled bool
}

// NewConfig builds a Config from the license key and account id. Enabled is
// derived from licenseKey being non-empty — mirrors telemetry.Init's
// cfg.Endpoint == "" check for OTEL.
func NewConfig(licenseKey, accountID string) Config {
	return Config{
		APIKey:    licenseKey,
		AccountID: accountID,
		Enabled:   licenseKey != "",
	}
}

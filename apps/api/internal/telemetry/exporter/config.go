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
	// Environment is config.Environment() ("development"/"staging"/"production"/
	// "test"), resolved once here and propagated onto every payload — staging
	// and production share the same New Relic account and dashboard, so every
	// custom event must self-identify to be filterable. Doesn't change at
	// runtime, hence resolved once at construction rather than re-read per
	// event.
	Environment string
}

// NewConfig builds a Config from the license key, account id and environment.
// Enabled is derived from licenseKey being non-empty — mirrors
// telemetry.Init's cfg.Endpoint == "" check for OTEL.
func NewConfig(licenseKey, accountID, environment string) Config {
	return Config{
		APIKey:      licenseKey,
		AccountID:   accountID,
		Enabled:     licenseKey != "",
		Environment: environment,
	}
}

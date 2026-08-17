package exporter

import "testing"

func TestNewConfig(t *testing.T) {
	tests := []struct {
		name        string
		licenseKey  string
		accountID   string
		wantEnabled bool
	}{
		{
			name:        "license key set enables the exporter",
			licenseKey:  "nr-license-key",
			accountID:   "8291202",
			wantEnabled: true,
		},
		{
			name:        "empty license key disables the exporter",
			licenseKey:  "",
			accountID:   "8291202",
			wantEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewConfig(tt.licenseKey, tt.accountID)

			if cfg.APIKey != tt.licenseKey {
				t.Errorf("APIKey = %q, want %q", cfg.APIKey, tt.licenseKey)
			}
			if cfg.AccountID != tt.accountID {
				t.Errorf("AccountID = %q, want %q", cfg.AccountID, tt.accountID)
			}
			if cfg.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", cfg.Enabled, tt.wantEnabled)
			}
		})
	}
}

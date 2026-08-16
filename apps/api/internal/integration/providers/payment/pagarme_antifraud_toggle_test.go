package payment

import (
	"testing"

	"livecart/apps/api/lib/config"
)

// TestPagarmeAntifraudDisabledForStore guards the per-store escape hatch that
// sends antifraud_enabled=false. It must be exact-match, tolerate whitespace,
// and never disable antifraud for a store that isn't explicitly listed.
func TestPagarmeAntifraudDisabledForStore(t *testing.T) {
	const cantodaart = "a5403331-afd4-40ff-9bbe-4aa6f5322aee"
	const other = "00000000-0000-0000-0000-000000000000"

	tests := []struct {
		name    string
		env     string
		storeID string
		want    bool
	}{
		{name: "empty allowlist disables nothing", env: "", storeID: cantodaart, want: false},
		{name: "listed store is disabled", env: cantodaart, storeID: cantodaart, want: true},
		{name: "unlisted store keeps antifraud", env: cantodaart, storeID: other, want: false},
		{name: "whitespace and multiple entries", env: " x , " + cantodaart + " ,y", storeID: cantodaart, want: true},
		{name: "blank store id never matches", env: cantodaart, storeID: "", want: false},
		{name: "no partial/substring match", env: cantodaart, storeID: cantodaart[:8], want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(string(config.PagarmeAntifraudDisabledStores), tt.env)
			if got := pagarmeAntifraudDisabledForStore(tt.storeID); got != tt.want {
				t.Errorf("pagarmeAntifraudDisabledForStore(%q) with env %q = %v, want %v",
					tt.storeID, tt.env, got, tt.want)
			}
		})
	}
}

package erp

import (
	"errors"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

// Erro transitório de lookup NÃO pode virar pendência (falso "missing") —
// bug de campo jul/2026: 429/timeout do Tiny marcava cadastro como faltando.
func TestHealthCheckItemStatuses(t *testing.T) {
	cases := []struct {
		name      string
		matchedID int64
		err       error
		want      providers.ERPHealthCheckStatus
	}{
		{"encontrado", 42, nil, providers.ERPHealthStatusOK},
		{"ausente de verdade", 0, nil, providers.ERPHealthStatusMissing},
		{"lookup falhou (transitório)", 0, errors.New("429 too many requests"), providers.ERPHealthStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := healthCheckItem(providers.ERPHealthFormaEnvio, "Correios", tc.matchedID, tc.err, "d", "p")
			if item.Status != tc.want {
				t.Fatalf("status = %q, esperado %q", item.Status, tc.want)
			}
		})
	}
}

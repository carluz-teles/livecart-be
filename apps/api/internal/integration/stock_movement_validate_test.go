package integration

// O portão sintático do resolve: false é resposta VÁLIDA e omitido não é.
//
// `landed` é *bool de propósito — com bool cru, {"landed": false} e um body
// vazio seriam indistinguíveis, e "não entrou" é exatamente a resposta que
// autoriza re-executar um POST de estoque. Um default silencioso aqui moveria
// estoque por omissão.

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

func TestResolveStockMovementValidate(t *testing.T) {
	sim, nao := true, false
	casos := []struct {
		nome  string
		req   ResolveStockMovementRequest
		valid bool
	}{
		{"entrou", ResolveStockMovementRequest{Landed: &sim}, true},
		{"não entrou — false é resposta válida", ResolveStockMovementRequest{Landed: &nao}, true},
		{"omitido é recusado", ResolveStockMovementRequest{}, false},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			err := c.req.Validate()
			if c.valid && err != nil {
				t.Fatalf("válido recusado: %v", err)
			}
			if !c.valid {
				errs, ok := err.(validation.Errors)
				if !ok || errs["landed"] == nil {
					t.Fatalf("esperava erro no campo landed, veio %v", err)
				}
			}
		})
	}
}

package integration

// O portão sintático da escolha de qual saldo do ERP o LiveCart espelha.
//
// O campo é ponteiro por um motivo concreto: com um `bool` puro, um PATCH que
// omitisse `useAvailableStock` chegaria no handler como `false` e DESLIGARIA a
// configuração sem ninguém ter pedido. É o mesmo buraco de zero-value que o
// ozzo tem com `Min` sem `Required`, e aqui o efeito é o lojista voltando a
// oferecer peça reservada por orçamento sem saber por quê.

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

func ptrBool(v bool) *bool { return &v }

func TestUpdateERPStockSourceRequestValidate(t *testing.T) {
	casos := []struct {
		nome        string
		req         UpdateERPStockSourceRequest
		querErro    bool
		campoNoErro string
	}{
		{
			nome: "ligar é válido",
			req:  UpdateERPStockSourceRequest{UseAvailableStock: ptrBool(true)},
		},
		{
			// O caso que o ponteiro existe para separar do campo ausente:
			// desligar explicitamente é uma escolha legítima e tem de passar.
			nome: "desligar explicitamente é válido",
			req:  UpdateERPStockSourceRequest{UseAvailableStock: ptrBool(false)},
		},
		{
			nome:        "campo ausente é recusado",
			req:         UpdateERPStockSourceRequest{},
			querErro:    true,
			campoNoErro: "useAvailableStock",
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			err := c.req.Validate()
			if !c.querErro {
				if err != nil {
					t.Fatalf("esperava válido, veio: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("esperava erro de validação; o campo omitido passaria como " +
					"false e desligaria a configuração em silêncio")
			}
			errs, ok := err.(validation.Errors)
			if !ok {
				t.Fatalf("esperava validation.Errors para virar 422 {error, fields}, veio %T", err)
			}
			if _, existe := errs[c.campoNoErro]; !existe {
				t.Errorf("erro não aponta o campo %q (veio %v) — o FE destaca o campo pela "+
					"chave json", c.campoNoErro, errs)
			}
		})
	}
}

// ToInput desreferencia o ponteiro; o valor tem de atravessar intacto nos dois
// sentidos, senão o usecase grava o oposto do que o lojista escolheu.
func TestUpdateERPStockSourceToInput(t *testing.T) {
	for _, escolhido := range []bool{true, false} {
		req := UpdateERPStockSourceRequest{UseAvailableStock: ptrBool(escolhido)}
		got := req.ToInput("loja-1", "integracao-9")
		if got.UseAvailableStock != escolhido {
			t.Errorf("UseAvailableStock = %v, quero %v", got.UseAvailableStock, escolhido)
		}
		if got.StoreID != "loja-1" || got.IntegrationID != "integracao-9" {
			t.Errorf("escopo perdido no ToInput: %+v", got)
		}
	}
}

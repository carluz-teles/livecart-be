package payment

// Recusa do antifraude não é recusa do banco, e o conselho ao comprador é
// diferente. Em 16/08/2026 uma venda de R$ 4,90 foi recusada três vezes com
// `acquirer_return_code: "0000"` e "Transação aprovada com sucesso" — o emissor
// autorizou e o antifraude do Pagar.me reprovou. O comprador lia "verifique os
// dados do cartão", conferia, e tentava de novo.

import "testing"

func TestAntifraudReproved(t *testing.T) {
	casos := []struct {
		nome      string
		antifraud map[string]any
		quer      bool
	}{
		{
			// A resposta real da transação de 16/08.
			nome: "reprovado de verdade",
			antifraud: map[string]any{
				"provider_name": "pagarme",
				"score":         "high",
				"status":        "reproved",
			},
			quer: true,
		},
		{
			nome:      "aprovado não é reprovação",
			antifraud: map[string]any{"status": "approved"},
			quer:      false,
		},
		{
			// Pagar.me não garante caixa. Comparar com == deixaria passar.
			nome:      "caixa alta não engana",
			antifraud: map[string]any{"status": "Reproved"},
			quer:      true,
		},
		{
			// Recusa do emissor: sem bloco de antifraude. Aqui o conselho certo é
			// outro cartão, e afirmar antifraude mandaria o comprador para o Pix
			// sem necessidade.
			nome: "sem antifraude é recusa do banco", antifraud: nil, quer: false,
		},
		{
			nome:      "bloco presente sem status",
			antifraud: map[string]any{"provider_name": "pagarme"},
			quer:      false,
		},
		{
			nome:      "status de outro tipo não quebra",
			antifraud: map[string]any{"status": 42},
			quer:      false,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := antifraudReproved(c.antifraud); got != c.quer {
				t.Errorf("antifraudReproved(%v) = %v, quero %v", c.antifraud, got, c.quer)
			}
		})
	}
}

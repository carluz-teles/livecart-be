package product

import "testing"

// Cobre a regra de estoque do sync ERP — em especial o "downgrade-only" da
// janela do guard (live com reserva / finalização em voo): reduções do lojista
// no Tiny refletem, aumentos (eco de reserva / inflação de finalização) são
// preservados para não disparar promoção fantasma da waitlist.
func TestResolveSyncedStock(t *testing.T) {
	cases := []struct {
		name       string
		local, erp int
		skip       bool
		want       int
	}{
		// Sync normal: sempre aplica o valor do ERP.
		{"normal aplica aumento", 5, 8, false, 8},
		{"normal aplica redução", 5, 2, false, 2},
		{"normal aplica igual", 5, 5, false, 5},

		// Fail-safe: preserva o local independentemente do valor.
		{"skip preserva mesmo com redução", 5, 2, true, 5},
		{"skip preserva mesmo com aumento", 5, 8, true, 5},

		// Saldo NEGATIVO no ERP nunca entra, em modo nenhum.
		//
		// O Tiny deixa o saldo passar de zero para baixo — gravado na bateria de
		// sandbox: lançar sobre 0 levou a -1. Negativo lá é saída que passou do
		// que existia, não estoque do lojista.
		//
		// Em 08/08 o Gabinete Gamer levou reserva de 3 sobre saldo 0, o Tiny foi
		// para -3 e ecoou -3. O valor entrou, a escrita bateu na constraint
		// `stock cannot be negative`, e o erro derrubou a sincronização INTEIRA
		// do produto — nome, preço, dimensões — que retentou quatro vezes e
		// desistiu.
		{"negativo no modo normal preserva o local", 5, -3, false, 5},
		{"negativo no downgrade preserva o local", 0, -3, false, 0},
		{"negativo com local zero preserva zero", 0, -1, false, 0},
		{"negativo profundo nao passa", 2, -99, false, 2},
		{"zero continua valido, so o negativo e barrado", 5, 0, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSyncedStock(tc.local, tc.erp, tc.skip)
			if got != tc.want {
				t.Fatalf("resolveSyncedStock(local=%d erp=%d skip=%v) = %d, esperado %d",
					tc.local, tc.erp, tc.skip, got, tc.want)
			}
		})
	}
}

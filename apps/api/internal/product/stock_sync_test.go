package product

import "testing"

// Cobre a regra de estoque do sync ERP — em especial o "downgrade-only" da
// janela do guard (live com reserva / finalização em voo): reduções do lojista
// no Tiny refletem, aumentos (eco de reserva / inflação de finalização) são
// preservados para não disparar promoção fantasma da waitlist.
func TestResolveSyncedStock(t *testing.T) {
	cases := []struct {
		name          string
		local, erp    int
		skip          bool
		downgradeOnly bool
		want          int
	}{
		// Sync normal: sempre aplica o valor do ERP.
		{"normal aplica aumento", 5, 8, false, false, 8},
		{"normal aplica redução", 5, 2, false, false, 2},
		{"normal aplica igual", 5, 5, false, false, 5},

		// Fail-safe: preserva o local independentemente do valor.
		{"skip preserva mesmo com redução", 5, 2, true, false, 5},
		{"skip preserva mesmo com aumento", 5, 8, true, false, 5},

		// Janela do guard (downgrade-only): o caso do bug relatado.
		{"guard: redução do lojista REFLETE", 5, 2, false, true, 2},
		{"guard: aumento é IGNORADO (anti-race)", 5, 8, false, true, 5},
		{"guard: valor igual é no-op", 5, 5, false, true, 5},
		{"guard: zerar estoque reflete", 3, 0, false, true, 0},

		// skip tem precedência sobre downgradeOnly.
		{"skip vence downgrade", 5, 2, true, true, 5},

		// Saldo NEGATIVO no ERP nunca entra, em modo nenhum.
		//
		// O Tiny deixa o saldo passar de zero para baixo — gravado na bateria de
		// sandbox: lançar sobre 0 levou a -1. Negativo lá é saída que passou do
		// que existia, não estoque do lojista.
		//
		// Em 08/08 o Gabinete Gamer levou reserva de 3 sobre saldo 0, o Tiny foi
		// para -3 e ecoou -3. O downgrade aceitou por ser "menor que o local", a
		// escrita bateu na constraint `stock cannot be negative`, e o erro
		// derrubou a sincronização INTEIRA do produto — quatro tentativas e
		// desistiu.
		{"negativo no modo normal preserva o local", 5, -3, false, false, 5},
		{"negativo no downgrade preserva o local", 0, -3, false, true, 0},
		{"negativo com local zero preserva zero", 0, -1, false, false, 0},
		{"negativo profundo nao passa", 2, -99, false, false, 2},
		{"zero continua valido, so o negativo e barrado", 5, 0, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSyncedStock(tc.local, tc.erp, tc.skip, tc.downgradeOnly)
			if got != tc.want {
				t.Fatalf("resolveSyncedStock(local=%d erp=%d skip=%v downgrade=%v) = %d, esperado %d",
					tc.local, tc.erp, tc.skip, tc.downgradeOnly, got, tc.want)
			}
		})
	}
}

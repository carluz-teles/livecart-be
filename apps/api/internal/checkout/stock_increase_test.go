package checkout

// O estoque limita o ACRÉSCIMO, não o total do carrinho.
//
// O bug de campo: produto com 5 unidades, comprador travado em 3. Ao tentar a
// quarta, o checkout respondia `{"error":"apenas 2 em estoque"}` — e havia 5.
//
// A conta comparava o TOTAL desejado com `products.stock`, que é o que SOBROU
// na prateleira. As 3 unidades dele já tinham saído dela quando entraram no
// carrinho, então sobravam 2, e `4 > 2` recusava. Ou seja: as unidades do
// próprio comprador contavam contra ele, e o teto real virava
// `ceil(estoque_inicial / 2) + 1`.
//
// O TETO por item (`limite de N por item`) continua sendo sobre o total — são
// duas perguntas diferentes, e confundi-las foi o erro.

import "testing"

func TestStockCoversIncrease(t *testing.T) {
	cases := []struct {
		name  string
		stock int
		added int
		want  bool
	}{
		// O caso de campo: 5 no total, 3 no carrinho → sobram 2, ele soma 1.
		{"o caso reportado: soma 1 com 2 na prateleira", 2, 1, true},
		{"soma exatamente o que sobrou", 2, 2, true},
		{"soma mais do que sobrou", 2, 3, false},

		{"prateleira cheia, soma 1", 5, 1, true},
		{"prateleira cheia, soma tudo", 5, 5, true},
		{"prateleira cheia, soma um a mais", 5, 6, false},

		// Diminuir quantidade nunca consulta estoque — devolve unidade, não tira.
		{"reduzir nao e bloqueado", 0, -2, true},
		{"quantidade igual nao e bloqueada", 0, 0, true},

		// Sem saldo, qualquer acréscimo é recusado (a fila decide depois).
		{"sem saldo recusa acrescimo", 0, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stockCoversIncrease(tc.stock, tc.added); got != tc.want {
				t.Errorf("stockCoversIncrease(stock=%d, added=%d) = %v, quero %v",
					tc.stock, tc.added, got, tc.want)
			}
		})
	}
}

// A regressão específica, escrita como o comprador a viveu: com 5 no estoque
// inicial, ele tem de conseguir chegar às 5 unidades, uma a uma.
func TestCompradorChegaAoEstoqueInteiroUmaAUma(t *testing.T) {
	const inicial = 5
	carrinho := 0

	for passo := 1; passo <= inicial; passo++ {
		prateleira := inicial - carrinho // o que products.stock teria neste ponto
		if !stockCoversIncrease(prateleira, 1) {
			t.Fatalf("travou com %d no carrinho e %d na prateleira — o comprador nao chega as %d unidades que existem",
				carrinho, prateleira, inicial)
		}
		carrinho++
	}

	if carrinho != inicial {
		t.Fatalf("chegou a %d, esperado %d", carrinho, inicial)
	}

	// E a sexta tem de ser recusada: aí o estoque acabou de verdade.
	if stockCoversIncrease(inicial-carrinho, 1) {
		t.Error("deixou passar a unidade que nao existe")
	}
}

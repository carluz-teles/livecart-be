package live

import "testing"

func sumQty(allocs []SessionAllocation) int {
	total := 0
	for _, a := range allocs {
		total += a.Quantity
	}
	return total
}

func TestAllocateBySession(t *testing.T) {
	cases := []struct {
		name      string
		finalQty  int
		additions []CartItemAddition
		want      []SessionAllocation
	}{
		{
			name:     "sem log fica sem sessao",
			finalQty: 3,
			want:     []SessionAllocation{{SessionID: "", Quantity: 3}},
		},
		{
			name:     "uma adicao de uma sessao",
			finalQty: 2,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 2, UnitPrice: 2500},
			},
			want: []SessionAllocation{{SessionID: "seg", Quantity: 2, UnitPrice: 2500}},
		},
		{
			// O caso que motiva a RN-29: 1un na segunda, 1un na quarta.
			// cart_items sozinho creditaria as duas à segunda.
			name:     "duas sessoes, uma unidade cada",
			finalQty: 2,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 1, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 1, UnitPrice: 2500},
			},
			want: []SessionAllocation{
				{SessionID: "seg", Quantity: 1, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 1, UnitPrice: 2500},
			},
		},
		{
			// Removeu depois: sai da adição mais recente.
			name:     "removeu uma unidade, a mais recente sai",
			finalQty: 2,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 2, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 1, UnitPrice: 2500},
			},
			want: []SessionAllocation{{SessionID: "seg", Quantity: 2, UnitPrice: 2500}},
		},
		{
			name:     "removeu tudo menos uma, sobra a primeira sessao",
			finalQty: 1,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 2, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 3, UnitPrice: 2500},
			},
			want: []SessionAllocation{{SessionID: "seg", Quantity: 1, UnitPrice: 2500}},
		},
		{
			// Alguém setou a quantidade acima do que o log conhece.
			name:     "quantidade final maior que o log sobra pra ultima sessao",
			finalQty: 5,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 1, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 1, UnitPrice: 2500},
			},
			want: []SessionAllocation{
				{SessionID: "seg", Quantity: 1, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 4, UnitPrice: 2500},
			},
		},
		{
			name:     "adicoes seguidas da mesma sessao e preco fundem",
			finalQty: 5,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 2, UnitPrice: 2500},
				{SessionID: "seg", Quantity: 3, UnitPrice: 2500},
			},
			want: []SessionAllocation{{SessionID: "seg", Quantity: 5, UnitPrice: 2500}},
		},
		{
			// Preço mudou entre as adições: NÃO funde, senão o pedido perderia
			// o preço praticado em cada momento.
			name:     "mesma sessao com precos diferentes nao funde",
			finalQty: 3,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 1, UnitPrice: 2500},
				{SessionID: "seg", Quantity: 2, UnitPrice: 1900},
			},
			want: []SessionAllocation{
				{SessionID: "seg", Quantity: 1, UnitPrice: 2500},
				{SessionID: "seg", Quantity: 2, UnitPrice: 1900},
			},
		},
		{
			name:     "adicao sem sessao convive com adicao de sessao",
			finalQty: 3,
			additions: []CartItemAddition{
				{SessionID: "", Quantity: 1, UnitPrice: 1000},
				{SessionID: "seg", Quantity: 2, UnitPrice: 1000},
			},
			want: []SessionAllocation{
				{SessionID: "", Quantity: 1, UnitPrice: 1000},
				{SessionID: "seg", Quantity: 2, UnitPrice: 1000},
			},
		},
		{
			name:     "quantidade final zero nao aloca nada",
			finalQty: 0,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 2, UnitPrice: 2500},
			},
			want: nil,
		},
		{
			name:     "adicao com quantidade zero e ignorada",
			finalQty: 2,
			additions: []CartItemAddition{
				{SessionID: "seg", Quantity: 0, UnitPrice: 2500},
				{SessionID: "qua", Quantity: 2, UnitPrice: 2500},
			},
			want: []SessionAllocation{{SessionID: "qua", Quantity: 2, UnitPrice: 2500}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AllocateBySession(tc.finalQty, tc.additions)

			if len(got) != len(tc.want) {
				t.Fatalf("alocacoes = %+v, quero %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("alocacao[%d] = %+v, quero %+v", i, got[i], tc.want[i])
				}
			}

			// O invariante que faz a metrica fechar: a soma tem de bater com a
			// quantidade final, sempre.
			if want := max(tc.finalQty, 0); sumQty(got) != want {
				t.Errorf("soma alocada = %d, quero %d — a receita por sessao nao fecharia com o total", sumQty(got), want)
			}
		})
	}
}

// Varre combinações para provar o invariante sem depender dos casos escritos à
// mão: qualquer quantidade final, contra qualquer log, sempre soma exato.
func TestAllocateBySessionInvariantHolds(t *testing.T) {
	logs := [][]CartItemAddition{
		nil,
		{{SessionID: "a", Quantity: 1, UnitPrice: 100}},
		{{SessionID: "a", Quantity: 3, UnitPrice: 100}, {SessionID: "b", Quantity: 2, UnitPrice: 100}},
		{{SessionID: "a", Quantity: 1, UnitPrice: 100}, {SessionID: "a", Quantity: 1, UnitPrice: 200}, {SessionID: "c", Quantity: 5, UnitPrice: 100}},
		{{SessionID: "", Quantity: 4, UnitPrice: 100}},
	}
	for _, additions := range logs {
		for finalQty := 0; finalQty <= 12; finalQty++ {
			got := AllocateBySession(finalQty, additions)
			if sumQty(got) != finalQty {
				t.Errorf("finalQty=%d log=%+v: soma = %d", finalQty, additions, sumQty(got))
			}
		}
	}
}

package checkout

// Só a parte SEGURADA move estoque; a parte em FILA não segura nada.
//
// O bug de campo: o PATCH do checkout calculava o delta sobre o TOTAL da linha
// e nunca mencionava waitlisted_quantity. Uma linha com 5 total e 3 em fila
// segura 2 unidades. Baixar para 2 mandava delta -3 ao estoque — creditava três
// unidades quando só duas haviam sido tiradas. A unidade inventada aparecia no
// LiveCart e no Tiny, e a linha ficava com 2 total sobre 3 em fila, ou seja
// disponível NEGATIVO, que os outros quatro pontos que calculam
// `quantity - waitlisted_quantity` leriam como número válido.
//
// A regra: ao reduzir, some primeiro a fila (o comprador abre mão dela sem
// custo para ninguém); só depois devolve estoque de verdade.

import "testing"

func TestSplitQuantityChange(t *testing.T) {
	cases := []struct {
		name                    string
		total, waitlisted, novo int
		wantHeld, wantWait      int
		wantDelta               int
	}{
		{
			// O caso de campo.
			name:  "reduz abaixo do segurado: fila zera e devolve so o que segurava",
			total: 5, waitlisted: 3, novo: 2,
			wantHeld: 2, wantWait: 0, wantDelta: 0,
		},
		{
			name:  "reduz mas ainda acima do segurado: so a fila encolhe",
			total: 5, waitlisted: 3, novo: 3,
			wantHeld: 2, wantWait: 1, wantDelta: 0,
		},
		{
			name:  "reduz ate abaixo do segurado: devolve a diferenca",
			total: 5, waitlisted: 3, novo: 1,
			wantHeld: 1, wantWait: 0, wantDelta: -1,
		},
		{
			name:  "linha sem fila: reduzir devolve tudo que reduziu",
			total: 4, waitlisted: 0, novo: 1,
			wantHeld: 1, wantWait: 0, wantDelta: -3,
		},
		{
			name:  "linha sem fila: aumentar segura o acrescimo",
			total: 2, waitlisted: 0, novo: 5,
			wantHeld: 5, wantWait: 0, wantDelta: 3,
		},
		{
			name:  "linha com fila: aumentar segura o acrescimo e preserva a fila",
			total: 5, waitlisted: 3, novo: 7,
			wantHeld: 4, wantWait: 3, wantDelta: 2,
		},
		{
			name:  "tudo em fila: reduzir nao devolve estoque nenhum",
			total: 3, waitlisted: 3, novo: 1,
			wantHeld: 0, wantWait: 1, wantDelta: 0,
		},
		{
			// Linha já corrompida pelo bug antigo. Não pode devolver estoque
			// que nunca saiu.
			name:  "linha inconsistente (fila maior que o total) nao inventa devolucao",
			total: 2, waitlisted: 3, novo: 1,
			wantHeld: 0, wantWait: 1, wantDelta: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			held, wait, delta := splitQuantityChange(tc.total, tc.waitlisted, tc.novo)
			if held != tc.wantHeld || wait != tc.wantWait || delta != tc.wantDelta {
				t.Errorf("split(total=%d, fila=%d, novo=%d) = (segurado %d, fila %d, delta %d), quero (%d, %d, %d)",
					tc.total, tc.waitlisted, tc.novo, held, wait, delta,
					tc.wantHeld, tc.wantWait, tc.wantDelta)
			}
			// Invariante que o bug rompia: total = segurado + fila, e a fila
			// nunca passa do total.
			if held+wait != tc.novo {
				t.Errorf("segurado(%d) + fila(%d) != total(%d)", held, wait, tc.novo)
			}
			if wait < 0 || held < 0 {
				t.Errorf("parcela negativa: segurado %d, fila %d", held, wait)
			}
		})
	}
}

// O estoque nunca pode receber de volta mais do que saiu dele.
func TestSplitNuncaDevolveMaisDoQueSegurava(t *testing.T) {
	for total := 0; total <= 8; total++ {
		for wait := 0; wait <= 8; wait++ {
			for novo := 1; novo <= 8; novo++ {
				heldBefore := total - wait
				if heldBefore < 0 {
					heldBefore = 0
				}
				_, _, delta := splitQuantityChange(total, wait, novo)
				if -delta > heldBefore {
					t.Fatalf("total=%d fila=%d novo=%d devolveu %d ao estoque, mas so segurava %d",
						total, wait, novo, -delta, heldBefore)
				}
			}
		}
	}
}

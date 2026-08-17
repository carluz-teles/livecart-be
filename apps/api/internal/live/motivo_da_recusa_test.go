package live

// O motivo da recusa precisa ser VERDADE, não um rótulo plausível.
//
// Um campo de log que explica errado é pior que campo nenhum: numa noite de
// live alguém vai agir com base nele. Estes testes amarram o motivo à regra que
// de fato barrou o comentário.

import "testing"

func TestMotivoDaRecusaExplicaARegraCerta(t *testing.T) {
	casos := []struct {
		texto  string
		motivo string
	}{
		{"", "texto vazio"},
		{"não quero", "negação ou cancelamento"},
		{"cancela o 1130", "negação ou cancelamento"},
		{"quanto custa o 1144", "pergunta conhecida"},
		{"Tem em estoque?", "pergunta conhecida"},
		{"valor 1000", "fala de preço sem quantidade explícita"},
		{"@medcesar R$3391,90", "fala de preço sem quantidade explícita"},
		{"Boa noite", "sem código e sem verbo de compra"},
		{"Esse cogumelo tem maior?", "sem código e sem verbo de compra"},
		{"Tb quero ver a cascata de luzes", "pediu para ver, não para comprar"},
		{"Não é o galho q quero", "negação na frase"},
		{"Manda a chuva que mandamos o sol kkkkkk", "verbo dentro de uma frase, sem código"},
	}

	for _, c := range casos {
		t.Run(c.texto, func(t *testing.T) {
			if itens := ParsePurchaseItems(c.texto); itens != nil {
				t.Fatalf("o caso virou pedido %+v — não é caso de recusa", itens)
			}
			if got := MotivoDaRecusa(c.texto); got != c.motivo {
				t.Errorf("motivo = %q; esperava %q. Um motivo errado no log faz "+
					"alguém procurar o defeito no lugar errado", got, c.motivo)
			}
		})
	}
}

// Todo comentário recusado do corpus tem motivo, e nenhum aceito é rotulado
// como recusa. Sem isto, um caminho novo de recusa nasceria mudo.
func TestTodaRecusaDoCorpusTemMotivo(t *testing.T) {
	for _, c := range casosDaLive {
		if c.esperado != nil {
			continue
		}
		if m := MotivoDaRecusa(c.texto); m == "" {
			t.Errorf("%q foi recusado sem motivo no log", c.texto)
		}
	}
}

func TestDescreveItens(t *testing.T) {
	casos := []struct {
		itens []PurchaseItem
		want  string
	}{
		{nil, ""},
		{[]PurchaseItem{{"1130", 2}}, "1130x2"},
		{[]PurchaseItem{{"1000", 5}, {"1005", 3}}, "1000x5 1005x3"},
		{[]PurchaseItem{{"", 2}}, "destaquex2"},
	}
	for _, c := range casos {
		if got := DescreveItens(c.itens); got != c.want {
			t.Errorf("DescreveItens(%+v) = %q; esperava %q", c.itens, got, c.want)
		}
	}
}

package erp

// A reconciliação contra o incidente que a motivou.
//
// Em 12/08/2026 o Gabinete Gamer terminou com uma unidade a mais no Tiny e o
// Perfume Cebolinha com uma a menos. O desvio existiu por horas sem que nada no
// sistema soubesse: foi o lojista quem percebeu, conferindo o ERP na mão.
//
// Estes testes reproduzem aquele estado final e exigem que a varredura aponte
// exatamente os dois produtos, com o sinal certo — e que fique calada quando
// está tudo em ordem, porque um detector que grita à toa é ignorado e vira
// ruído em duas semanas.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

type leitorFake struct {
	posicoes []StockPosition
	err      error
}

func (l leitorFake) ListStockPositionsForReconciliation(context.Context, string, string) ([]StockPosition, error) {
	return l.posicoes, l.err
}

// erpFake responde `disponivel` — que é o único saldo que o provider real
// entrega hoje. O físico não chega mais até aqui.
type erpFake struct {
	saldo map[string]int
	falha map[string]bool
}

func (e erpFake) GetProductStock(_ context.Context, externalID string) (int, error) {
	if e.falha[externalID] {
		return 0, errors.New("HTTP 500 do Tiny")
	}
	return e.saldo[externalID], nil
}

// O estado final de 12/08: sete produtos, dois errados, nenhuma reserva viva
// (todos os carrinhos já cancelados).
func TestReconciliacaoApontaOsDoisProdutosDoIncidente(t *testing.T) {
	leitor := leitorFake{posicoes: []StockPosition{
		{ProductID: "p1", Name: "Gabinete Gamer", ExternalID: "362828569", LocalStock: 5},
		{ProductID: "p2", Name: "Perfume Cebolinha", ExternalID: "362829084", LocalStock: 5},
		{ProductID: "p3", Name: "Café em Grão", ExternalID: "362829514", LocalStock: 5},
		{ProductID: "p4", Name: "Playstation 5", ExternalID: "362829312", LocalStock: 5},
	}}
	tiny := erpFake{saldo: map[string]int{
		"362828569": 6, // uma unidade inventada
		"362829084": 4, // uma unidade perdida
		"362829514": 5,
		"362829312": 5,
	}}

	rel, err := ReconcileStockAgainstERP(context.Background(), zap.NewNop(), leitor, tiny, "loja-1", "tiny")
	if err != nil {
		t.Fatalf("reconciliando: %v", err)
	}

	if rel.Checked != 4 {
		t.Errorf("conferiu %d produtos, quero 4", rel.Checked)
	}
	if len(rel.Divergences) != 2 {
		t.Fatalf("achou %d divergências, quero 2 (Gabinete +1 e Perfume -1): %+v",
			len(rel.Divergences), rel.Divergences)
	}

	porID := map[string]StockDivergence{}
	for _, d := range rel.Divergences {
		porID[d.ExternalID] = d
	}

	gab, ok := porID["362828569"]
	if !ok {
		t.Fatal("não apontou o Gabinete Gamer — foi ele que fechou com 6 onde havia 5")
	}
	if gab.Delta != 1 {
		t.Errorf("Gabinete: delta %d, quero +1 (unidade a MAIS no ERP)", gab.Delta)
	}

	perf, ok := porID["362829084"]
	if !ok {
		t.Fatal("não apontou o Perfume Cebolinha — fechou com 4 onde havia 5")
	}
	if perf.Delta != -1 {
		t.Errorf("Perfume: delta %d, quero -1 (unidade a MENOS no ERP)", perf.Delta)
	}
}

// Carrinho aberto NÃO é divergência, e agora isso sai de graça: o item no
// carrinho desconta do contador local aqui e, porque o carrinho virou pedido de
// venda, desconta do `disponivel` lá. Os dois lados andam juntos.
//
// Este teste substituiu um que subtraía `held` de `local` para chegar no mesmo
// lugar. A subtração era necessária quando as reservas eram saídas manuais e
// mexiam no saldo FÍSICO; ela sumiu junto com elas.
func TestCarrinhoAbertoNaoEhDivergencia(t *testing.T) {
	// Console com 8 no ERP; 3 unidades já em carrinhos abertos, então o
	// disponível de lá é 5 e o contador local também.
	leitor := leitorFake{posicoes: []StockPosition{
		{ProductID: "p1", Name: "Console", ExternalID: "E1", LocalStock: 5},
	}}
	tiny := erpFake{saldo: map[string]int{"E1": 5}}

	rel, err := ReconcileStockAgainstERP(context.Background(), zap.NewNop(), leitor, tiny, "loja-1", "tiny")
	if err != nil {
		t.Fatalf("reconciliando: %v", err)
	}
	if len(rel.Divergences) != 0 {
		t.Errorf("acusou divergência com carrinho aberto legítimo: %+v", rel.Divergences)
	}
}

// O FÍSICO chegando aqui É divergência, e tem de aparecer como tal.
//
// Se alguma regressão fizer o provider voltar a devolver `saldo` em vez de
// `disponivel`, toda loja com carrinho aberto passa a divergir — e é assim que
// se descobre, em vez de descobrir vendendo o que já tem dono.
func TestSaldoFisicoNoLugarDoDisponivelApareceComoDivergencia(t *testing.T) {
	leitor := leitorFake{posicoes: []StockPosition{
		{ProductID: "p1", Name: "Console", ExternalID: "E1", LocalStock: 5},
	}}
	tiny := erpFake{saldo: map[string]int{"E1": 8}} // 8 é o físico; 5 é o disponível

	rel, err := ReconcileStockAgainstERP(context.Background(), zap.NewNop(), leitor, tiny, "loja-1", "tiny")
	if err != nil {
		t.Fatalf("reconciliando: %v", err)
	}
	if len(rel.Divergences) != 1 || rel.Divergences[0].Delta != 3 {
		t.Errorf("quero uma divergência de +3 (o físico contando as 3 unidades já "+
			"comprometidas), veio %+v", rel.Divergences)
	}
}

// Tudo certo tem de ser silêncio total.
func TestReconciliacaoSilenciosaQuandoEstaTudoCerto(t *testing.T) {
	leitor := leitorFake{posicoes: []StockPosition{
		{ProductID: "p1", Name: "A", ExternalID: "E1", LocalStock: 5},
		{ProductID: "p2", Name: "B", ExternalID: "E2", LocalStock: 3},
	}}
	tiny := erpFake{saldo: map[string]int{"E1": 5, "E2": 3}}

	rel, err := ReconcileStockAgainstERP(context.Background(), zap.NewNop(), leitor, tiny, "loja-1", "tiny")
	if err != nil {
		t.Fatalf("reconciliando: %v", err)
	}
	if len(rel.Divergences) != 0 {
		t.Errorf("um detector que grita à toa é ignorado em duas semanas: %+v", rel.Divergences)
	}
	if rel.Checked != 2 {
		t.Errorf("conferiu %d, quero 2", rel.Checked)
	}
}

// ERP fora do ar não vira divergência — vira produto pulado. Confundir os dois
// encheria o relatório de ruído exatamente quando ele precisa ser legível.
func TestFalhaDoERPNaoViraDivergencia(t *testing.T) {
	leitor := leitorFake{posicoes: []StockPosition{
		{ProductID: "p1", Name: "A", ExternalID: "E1", LocalStock: 5},
		{ProductID: "p2", Name: "B", ExternalID: "E2", LocalStock: 5},
	}}
	tiny := erpFake{
		saldo: map[string]int{"E1": 5},
		falha: map[string]bool{"E2": true},
	}

	rel, err := ReconcileStockAgainstERP(context.Background(), zap.NewNop(), leitor, tiny, "loja-1", "tiny")
	if err != nil {
		t.Fatalf("reconciliando: %v", err)
	}
	if len(rel.Divergences) != 0 {
		t.Errorf("tratou falha de consulta como divergência: %+v", rel.Divergences)
	}
	if rel.Skipped != 1 {
		t.Errorf("pulados = %d, quero 1", rel.Skipped)
	}
	if rel.Checked != 1 {
		t.Errorf("conferidos = %d, quero 1", rel.Checked)
	}
}

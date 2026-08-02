package live

// Testes da repartição PROJETADA por transmissão (Fatia 5).
//
// O invariante testado em todos os casos é o mesmo, e é o que sustenta a métrica
// em dois níveis: SUM(receita das sessões) == SUM(quantity * unit_price) dos
// itens. Se algum caso quebrar isso, a tela do lojista mostra uma soma de
// transmissões que não bate com o total do evento — e a feature perde a
// confiança no primeiro dia.

import (
	"sort"
	"testing"
)

func totalDosItens(items []OpenCartItem) int64 {
	var v int64
	for _, i := range items {
		v += int64(i.Quantity) * i.UnitPrice
	}
	return v
}

func somaProjetada(p []SessionProjection) int64 {
	var v int64
	for _, s := range p {
		v += s.RevenueCents
	}
	return v
}

func porSessao(p []SessionProjection) map[string]SessionProjection {
	m := map[string]SessionProjection{}
	for _, s := range p {
		m[s.SessionID] = s
	}
	return m
}

func add(cart, product, session string, qty int, price int64) CartItemAdditionRow {
	return CartItemAdditionRow{
		CartID:    cart,
		ProductID: product,
		CartItemAddition: CartItemAddition{
			SessionID: session,
			Quantity:  qty,
			UnitPrice: price,
		},
	}
}

func TestProjectBySessionSplitsBetweenBroadcasts(t *testing.T) {
	// Semana Black: 1 vestido na segunda, 1 na quarta, no MESMO carrinho.
	items := []OpenCartItem{{CartID: "c1", ProductID: "vestido", Quantity: 2, UnitPrice: 2500}}
	additions := []CartItemAdditionRow{
		add("c1", "vestido", "segunda", 1, 2500),
		add("c1", "vestido", "quarta", 1, 2500),
	}

	got := porSessao(ProjectBySession(items, additions))

	if got["segunda"].RevenueCents != 2500 || got["segunda"].Units != 1 {
		t.Errorf("segunda = %+v, quero 1un/2500", got["segunda"])
	}
	if got["quarta"].RevenueCents != 2500 || got["quarta"].Units != 1 {
		t.Errorf("quarta = %+v, quero 1un/2500", got["quarta"])
	}
	if s := somaProjetada(ProjectBySession(items, additions)); s != totalDosItens(items) {
		t.Errorf("invariante quebrado: soma %d != total %d", s, totalDosItens(items))
	}
}

func TestProjectBySessionUsesCartPriceNotLogPrice(t *testing.T) {
	// O lojista mudou o preço depois da adição. A verdade da receita é o preço
	// ATUAL do carrinho — é o que o selamento congela e o que
	// cart_product_total_cents soma. Usar o preço do log faria projetado e
	// confirmado divergirem no instante do pagamento.
	items := []OpenCartItem{{CartID: "c1", ProductID: "vestido", Quantity: 2, UnitPrice: 3000}}
	additions := []CartItemAdditionRow{
		add("c1", "vestido", "segunda", 1, 2500),
		add("c1", "vestido", "quarta", 1, 2500),
	}

	proj := ProjectBySession(items, additions)
	if s := somaProjetada(proj); s != 6000 {
		t.Errorf("soma = %d, quero 6000 (preço do carrinho, não o do log)", s)
	}
	if got := porSessao(proj)["segunda"].RevenueCents; got != 3000 {
		t.Errorf("segunda = %d, quero 3000", got)
	}
}

func TestProjectBySessionRemocaoSaiDaAdicaoMaisRecente(t *testing.T) {
	// Log com 3 unidades, carrinho com 2: o comprador removeu 1. Sai da adição
	// mais recente (quarta), que é a semântica intuitiva de remoção.
	items := []OpenCartItem{{CartID: "c1", ProductID: "vestido", Quantity: 2, UnitPrice: 1000}}
	additions := []CartItemAdditionRow{
		add("c1", "vestido", "segunda", 2, 1000),
		add("c1", "vestido", "quarta", 1, 1000),
	}

	proj := ProjectBySession(items, additions)
	got := porSessao(proj)
	if got["segunda"].Units != 2 {
		t.Errorf("segunda = %d un, quero 2", got["segunda"].Units)
	}
	if _, ok := got["quarta"]; ok {
		t.Errorf("quarta não devia aparecer: %+v", got["quarta"])
	}
	if s := somaProjetada(proj); s != totalDosItens(items) {
		t.Errorf("invariante quebrado: soma %d != total %d", s, totalDosItens(items))
	}
}

func TestProjectBySessionSobraVaiParaAUltimaSessao(t *testing.T) {
	// Carrinho com 5, log com 2: alguém setou a quantidade por um caminho que
	// não passa pelo log (o AddCartItem do checkout, hoje). O excedente vai
	// para a última sessão vista — a melhor evidência que existe — e o total
	// continua fechando.
	items := []OpenCartItem{{CartID: "c1", ProductID: "vestido", Quantity: 5, UnitPrice: 1000}}
	additions := []CartItemAdditionRow{
		add("c1", "vestido", "segunda", 1, 1000),
		add("c1", "vestido", "quarta", 1, 1000),
	}

	proj := ProjectBySession(items, additions)
	got := porSessao(proj)
	if got["quarta"].Units != 4 {
		t.Errorf("quarta = %d un, quero 4 (1 do log + 3 de sobra)", got["quarta"].Units)
	}
	if s := somaProjetada(proj); s != totalDosItens(items) {
		t.Errorf("invariante quebrado: soma %d != total %d", s, totalDosItens(items))
	}
}

func TestProjectBySessionBaldeSemTransmissao(t *testing.T) {
	// Item sem log nenhum (posto pelo painel) e item com adição sem sessão.
	// Os dois caem no balde "" — que TEM de existir no resultado, senão a soma
	// das transmissões fica menor que o total do evento.
	items := []OpenCartItem{
		{CartID: "c1", ProductID: "brinde", Quantity: 1, UnitPrice: 700},
		{CartID: "c1", ProductID: "vestido", Quantity: 1, UnitPrice: 2500},
	}
	additions := []CartItemAdditionRow{
		add("c1", "vestido", "", 1, 2500),
	}

	proj := ProjectBySession(items, additions)
	got := porSessao(proj)
	if got[""].RevenueCents != 3200 {
		t.Errorf(`balde "" = %d, quero 3200`, got[""].RevenueCents)
	}
	if s := somaProjetada(proj); s != totalDosItens(items) {
		t.Errorf("invariante quebrado: soma %d != total %d", s, totalDosItens(items))
	}
}

func TestProjectBySessionContaCarrinhosDistintos(t *testing.T) {
	items := []OpenCartItem{
		{CartID: "c1", ProductID: "vestido", Quantity: 1, UnitPrice: 1000},
		{CartID: "c1", ProductID: "bolsa", Quantity: 1, UnitPrice: 2000},
		{CartID: "c2", ProductID: "vestido", Quantity: 1, UnitPrice: 1000},
	}
	additions := []CartItemAdditionRow{
		add("c1", "vestido", "segunda", 1, 1000),
		add("c1", "bolsa", "segunda", 1, 2000),
		add("c2", "vestido", "segunda", 1, 1000),
	}

	got := porSessao(ProjectBySession(items, additions))
	if got["segunda"].OpenCarts != 2 {
		t.Errorf("openCarts = %d, quero 2 (dois carrinhos, três itens)", got["segunda"].OpenCarts)
	}
}

func TestProjectBySessionMultiplosCarrinhosNaoSeMisturam(t *testing.T) {
	// O log é chaveado por (carrinho, produto): a adição do carrinho da Maria
	// não pode alimentar a alocação do carrinho da Joana, mesmo sendo o mesmo
	// produto.
	items := []OpenCartItem{
		{CartID: "maria", ProductID: "vestido", Quantity: 1, UnitPrice: 1000},
		{CartID: "joana", ProductID: "vestido", Quantity: 1, UnitPrice: 1000},
	}
	additions := []CartItemAdditionRow{
		add("maria", "vestido", "segunda", 1, 1000),
		add("joana", "vestido", "quarta", 1, 1000),
	}

	got := porSessao(ProjectBySession(items, additions))
	if got["segunda"].Units != 1 || got["quarta"].Units != 1 {
		t.Errorf("segunda=%d quarta=%d, quero 1 e 1", got["segunda"].Units, got["quarta"].Units)
	}
}

func TestProjectBySessionResultadoDeterministico(t *testing.T) {
	// Sem ordem estável, a lista mudaria de posição a cada request — e o
	// painel piscaria linhas sem nada ter mudado.
	items := []OpenCartItem{
		{CartID: "c1", ProductID: "a", Quantity: 1, UnitPrice: 100},
		{CartID: "c1", ProductID: "b", Quantity: 1, UnitPrice: 100},
		{CartID: "c1", ProductID: "c", Quantity: 1, UnitPrice: 100},
	}
	additions := []CartItemAdditionRow{
		add("c1", "a", "s1", 1, 100),
		add("c1", "b", "s2", 1, 100),
		add("c1", "c", "s3", 1, 100),
	}

	first := ProjectBySession(items, additions)
	for i := 0; i < 20; i++ {
		again := ProjectBySession(items, additions)
		if len(again) != len(first) {
			t.Fatalf("tamanho instável: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j].SessionID != first[j].SessionID {
				t.Fatalf("ordem instável na posição %d: %q vs %q", j, again[j].SessionID, first[j].SessionID)
			}
		}
	}

	ids := make([]string, len(first))
	for i, p := range first {
		ids[i] = p.SessionID
	}
	sort.Strings(ids)
	if len(ids) != 3 {
		t.Errorf("sessões = %v, quero 3", ids)
	}
}

func TestProjectBySessionSemItens(t *testing.T) {
	if got := ProjectBySession(nil, nil); len(got) != 0 {
		t.Errorf("evento sem carrinho aberto devia projetar nada, veio %+v", got)
	}
}

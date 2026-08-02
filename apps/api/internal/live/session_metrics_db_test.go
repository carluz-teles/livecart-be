package live

// O invariante da Fatia 5 contra Postgres real: a soma das transmissões bate
// EXATAMENTE com o total do evento.
//
// O que estes testes provam, e que nenhum teste de unidade prova, é que o
// PREDICADO das duas metades é o mesmo. A repartição pode estar perfeita e a
// soma ainda não fechar se a query por sessão olhar um conjunto de carrinhos
// diferente do que GetEventStats.projected_revenue soma — que foi exatamente o
// defeito da GetSessionStats antiga (ela incluía carrinho pago, cancelado e
// estornado; o evento só inclui active/checkout).
//
// Por isso a asserção final nunca é contra um número escrito à mão: é sempre
// contra o que a query do EVENTO devolve, sobre o mesmo dado.

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func storeOf(t *testing.T, eventID string) string {
	t.Helper()
	var storeID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT store_id::text FROM live_events WHERE id = $1::uuid`, eventID,
	).Scan(&storeID); err != nil {
		t.Fatalf("resolver store: %v", err)
	}
	return storeID
}

// projectedDoEvento é o número que a tela do evento mostra hoje.
func projectedDoEvento(t *testing.T, eventID string) int64 {
	t.Helper()
	stats, err := testRepo.GetEventStats(context.Background(), eventID)
	if err != nil {
		t.Fatalf("GetEventStats: %v", err)
	}
	return stats.ProjectedRevenue
}

func TestSessionMetricsProjectionMatchesEventTotal(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	segunda := seedSession(t, eventID, 1)
	quarta := seedSession(t, eventID, 2)

	vestido := seedProduct(t, eventID, 2500)
	bolsa := seedProduct(t, eventID, 8000)

	maria, _ := getOrCreate(t, eventID, "maria")
	joana, _ := getOrCreate(t, eventID, "joana")

	add := func(cartID, session, product string, qty int, price int64) {
		t.Helper()
		if err := testRepo.AddCartItem(ctx, AddCartItemParams{
			CartID: cartID, ProductID: product, SessionID: session,
			Quantity: qty, UnitPrice: price,
		}); err != nil {
			t.Fatalf("AddCartItem: %v", err)
		}
	}

	// Maria comprou o mesmo vestido na segunda e na quarta — o caso que a
	// atribuição por carts.session_id errava — e uma bolsa na quarta.
	add(maria.ID, segunda, vestido, 1, 2500)
	add(maria.ID, quarta, vestido, 1, 2500)
	add(maria.ID, quarta, bolsa, 1, 8000)
	// Joana só na segunda.
	add(joana.ID, segunda, vestido, 2, 2500)
	// Brinde posto pelo painel: adição SEM transmissão.
	add(joana.ID, "", bolsa, 1, 8000)

	svc := NewService(testRepo, zap.NewNop())
	metrics, err := svc.GetSessionMetrics(ctx, eventID, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics: %v", err)
	}

	// A quebra por transmissão.
	got := map[string]SessionMetricsOutput{}
	for _, s := range metrics.Sessions {
		got[s.SessionID] = s
	}
	// Segunda: 1 vestido da Maria + 2 da Joana = 3 × 2500.
	if got[segunda].ProjectedRevenue != 7500 || got[segunda].ProjectedUnits != 3 {
		t.Errorf("segunda = %d cents / %d un, quero 7500/3",
			got[segunda].ProjectedRevenue, got[segunda].ProjectedUnits)
	}
	if got[segunda].OpenCarts != 2 {
		t.Errorf("segunda openCarts = %d, quero 2", got[segunda].OpenCarts)
	}
	// Quarta: 1 vestido + 1 bolsa da Maria.
	if got[quarta].ProjectedRevenue != 10500 {
		t.Errorf("quarta = %d cents, quero 10500", got[quarta].ProjectedRevenue)
	}
	// O brinde sem transmissão não pode sumir.
	if metrics.Unattributed == nil {
		t.Fatalf("balde sem transmissão ausente — a soma não fecharia")
	}
	if metrics.Unattributed.ProjectedRevenue != 8000 {
		t.Errorf("sem transmissão = %d, quero 8000", metrics.Unattributed.ProjectedRevenue)
	}

	// O invariante.
	if metrics.ProjectedRevenue != projectedDoEvento(t, eventID) {
		t.Errorf("soma das sessões (%d) != projetado do evento (%d)",
			metrics.ProjectedRevenue, projectedDoEvento(t, eventID))
	}
}

func TestSessionMetricsProjectionIgnoresClosedCarts(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	segunda := seedSession(t, eventID, 1)
	vestido := seedProduct(t, eventID, 2500)

	viva, _ := getOrCreate(t, eventID, "maria")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: viva.ID, ProductID: vestido, SessionID: segunda, Quantity: 2, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}

	// Carrinho que morreu. O projetado do evento não o conta; a quebra por
	// sessão também não pode contar, ou a soma passa do total.
	morta, _ := getOrCreate(t, eventID, "joana")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: morta.ID, ProductID: vestido, SessionID: segunda, Quantity: 5, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET status = 'expired' WHERE id = $1::uuid`, morta.ID,
	); err != nil {
		t.Fatalf("expirar cart: %v", err)
	}

	svc := NewService(testRepo, zap.NewNop())
	metrics, err := svc.GetSessionMetrics(ctx, eventID, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics: %v", err)
	}

	if metrics.ProjectedRevenue != 5000 {
		t.Errorf("projetado = %d, quero 5000 (o carrinho expirado não entra)", metrics.ProjectedRevenue)
	}
	if metrics.ProjectedRevenue != projectedDoEvento(t, eventID) {
		t.Errorf("soma das sessões (%d) != projetado do evento (%d)",
			metrics.ProjectedRevenue, projectedDoEvento(t, eventID))
	}
}

// O carrinho PAGO não é expectativa de venda.
//
// carts.status nunca vira 'paid' (UpdateCartPayment mexe só em payment_status),
// então o carrinho pago fica em 'checkout' para sempre. Enquanto o projetado
// olhava só o status, o MESMO dinheiro aparecia duas vezes na tela do evento:
// uma como receita confirmada (a venda) e outra como "projetado" — que a
// tooltip define como "carrinhos abertos que ainda não foram pagos".
//
// O teste prende as duas pontas ao mesmo tempo: o número do evento tem de
// excluir o pago E a soma das transmissões tem de continuar fechando com ele.
// Excluir só de um lado é o modo de falha que a Fatia 5 existe para impedir.
func TestSessionMetricsProjectionExcludesPaidCarts(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	segunda := seedSession(t, eventID, 1)
	vestido := seedProduct(t, eventID, 2500)

	// Maria ainda não pagou: R$ 50 de expectativa legítima.
	maria, _ := getOrCreate(t, eventID, "maria")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: maria.ID, ProductID: vestido, SessionID: segunda, Quantity: 2, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}

	// Joana pagou R$ 100. O carrinho dela continua em 'checkout'.
	joana, _ := getOrCreate(t, eventID, "joana")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: joana.ID, ProductID: vestido, SessionID: segunda, Quantity: 4, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET status = 'checkout', payment_status = 'paid', paid_at = now()
		 WHERE id = $1::uuid`, joana.ID,
	); err != nil {
		t.Fatalf("pagar cart: %v", err)
	}

	if got := projectedDoEvento(t, eventID); got != 5000 {
		t.Errorf("projetado do evento = %d, quero 5000 (só o carrinho da Maria; o pago já é receita confirmada)", got)
	}

	stats, err := testRepo.GetEventStats(ctx, eventID)
	if err != nil {
		t.Fatalf("GetEventStats: %v", err)
	}
	if stats.OpenCarts != 1 {
		t.Errorf("openCarts = %d, quero 1 — carrinho pago não está aberto", stats.OpenCarts)
	}

	svc := NewService(testRepo, zap.NewNop())
	metrics, err := svc.GetSessionMetrics(ctx, eventID, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics: %v", err)
	}

	got := map[string]SessionMetricsOutput{}
	for _, s := range metrics.Sessions {
		got[s.SessionID] = s
	}
	if got[segunda].ProjectedRevenue != 5000 || got[segunda].ProjectedUnits != 2 {
		t.Errorf("segunda = %d cents / %d un, quero 5000/2", got[segunda].ProjectedRevenue, got[segunda].ProjectedUnits)
	}
	if got[segunda].OpenCarts != 1 {
		t.Errorf("segunda openCarts = %d, quero 1", got[segunda].OpenCarts)
	}

	// O invariante: o predicado dos dois níveis é o MESMO.
	if metrics.ProjectedRevenue != projectedDoEvento(t, eventID) {
		t.Errorf("soma das sessões (%d) != projetado do evento (%d)",
			metrics.ProjectedRevenue, projectedDoEvento(t, eventID))
	}
}

func TestSessionMetricsListsSilentSessionsInCampaignOrder(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	// Criadas em ordem invertida de sequência de propósito: ListSessionsByEvent
	// devolve por created_at DESC, e relatório se lê da 1ª transmissão para a
	// última.
	terceira := seedSession(t, eventID, 3)
	primeira := seedSession(t, eventID, 1)
	segunda := seedSession(t, eventID, 2)

	vestido := seedProduct(t, eventID, 2500)
	maria, _ := getOrCreate(t, eventID, "maria")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: maria.ID, ProductID: vestido, SessionID: segunda, Quantity: 1, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}

	svc := NewService(testRepo, zap.NewNop())
	metrics, err := svc.GetSessionMetrics(ctx, eventID, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics: %v", err)
	}

	want := []string{primeira, segunda, terceira}
	if len(metrics.Sessions) != len(want) {
		t.Fatalf("sessões = %d, quero %d — transmissão sem venda tem de aparecer zerada",
			len(metrics.Sessions), len(want))
	}
	for i, id := range want {
		if metrics.Sessions[i].SessionID != id {
			t.Errorf("posição %d = %s, quero %s (ordem da campanha)", i, metrics.Sessions[i].SessionID, id)
		}
	}
	if metrics.Sessions[0].ProjectedRevenue != 0 || metrics.Sessions[2].ProjectedRevenue != 0 {
		t.Errorf("transmissão sem venda devia vir zerada, veio %+v / %+v",
			metrics.Sessions[0], metrics.Sessions[2])
	}
	if metrics.Unattributed != nil {
		t.Errorf("nada sem transmissão, mas o balde veio: %+v", metrics.Unattributed)
	}
}

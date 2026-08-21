package live

// Clientes VIP / F3 — atribuição da métrica de VENDA por evento de ORIGEM.
//
// A venda confirmada credita cada unidade ao evento da SESSÃO que a vendeu
// (order_items.session_id -> live_sessions.event_id), não ao evento âncora do
// carrinho. Provas contra o Postgres real:
//
//   1. PARIDADE — carrinho normal de UM evento: o número é idêntico ao modelo
//      antigo (COALESCE cai em orders.event_id), inclusive o item posto pelo
//      painel (session NULL) que continua no evento pelo fallback. E o
//      invariante da Fatia 5 segue de pé: soma das sessões == confirmado do
//      evento.
//   2. CORRETUDE VIP — um pedido âncora no evento X com itens vendidos em X e em
//      Y aparece repartido: X recebe só a parte de X, Y recebe só a parte de Y.
//      É o "produto que ele adicionou no evento Y conta para o evento Y".

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// sealItem é uma linha de order_items a sinalar (espelha o que AllocateBySession
// produziria: uma linha por (produto, sessão)).
type sealItem struct {
	productID   string
	productName string
	qty         int
	price       int64
	sessionID   string // "" => session_id NULL (adição sem transmissão)
}

// sealPaidOrder materializa um pedido PAGO para um carrinho, com order_items já
// repartidos por sessão. eventID é o evento ÂNCORA do pedido (orders.event_id),
// como o selamento grava; a origem de cada unidade vem do session_id do item.
func sealPaidOrder(t *testing.T, cartID, storeID, eventID string, items []sealItem) {
	t.Helper()
	ctx := context.Background()
	var total int64
	for _, it := range items {
		total += int64(it.qty) * it.price
	}
	var orderID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status, total_cents, paid_total_cents, paid_at)
		 VALUES ($1::uuid, (floor(random()*90000)+10000)::int, $2::uuid, $3::uuid, 'paid', $4, $4, now())
		 RETURNING id::text`,
		cartID, storeID, eventID, total,
	).Scan(&orderID); err != nil {
		t.Fatalf("seal order: %v", err)
	}
	for _, it := range items {
		var sess interface{}
		if it.sessionID != "" {
			sess = it.sessionID
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price, session_id)
			 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid)`,
			orderID, it.productID, it.productName, it.qty, it.price, sess,
		); err != nil {
			t.Fatalf("seal order_item: %v", err)
		}
	}
}

// TestConfirmedMetricsParityForSingleEventCart — o carrinho de um único evento
// não muda: o COALESCE cai em orders.event_id e o item de painel (session NULL)
// continua no evento pelo fallback. O invariante da Fatia 5 segue firme.
func TestConfirmedMetricsParityForSingleEventCart(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	s1 := seedSession(t, eventID, 1)
	s2 := seedSession(t, eventID, 2)
	vestido := seedProduct(t, eventID, 2500)

	cart, _ := getOrCreate(t, eventID, "maria")
	// Pedido pago: 1 na s1, 2 na s2, e 1 posto pelo painel (sem transmissão).
	sealPaidOrder(t, cart.ID, storeID, eventID, []sealItem{
		{vestido, "Vestido", 1, 2500, s1},
		{vestido, "Vestido", 2, 2500, s2},
		{vestido, "Vestido", 1, 2500, ""}, // session NULL -> fallback pro evento
	})

	stats, err := testRepo.GetEventStats(ctx, eventID)
	if err != nil {
		t.Fatalf("GetEventStats: %v", err)
	}
	// 4 unidades × 2500 = 10000, TODAS no evento (o painel cai no fallback).
	if stats.ConfirmedRevenue != 10000 {
		t.Errorf("confirmed = %d, quero 10000 (item de painel entra pelo fallback)", stats.ConfirmedRevenue)
	}
	if stats.TotalProductsSold != 4 {
		t.Errorf("vendidos = %d, quero 4", stats.TotalProductsSold)
	}

	// Invariante Fatia 5: soma das transmissões == confirmado do evento.
	confirmed, err := testRepo.ListSessionConfirmedRevenueByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("ListSessionConfirmedRevenueByEvent: %v", err)
	}
	var somaRev int64
	var somaUn int
	for _, c := range confirmed {
		somaRev += c.RevenueCents
		somaUn += c.SoldUnits
	}
	if somaRev != stats.ConfirmedRevenue {
		t.Errorf("soma das sessões (%d) != confirmado do evento (%d)", somaRev, stats.ConfirmedRevenue)
	}
	if somaUn != stats.TotalProductsSold {
		t.Errorf("soma de unidades das sessões (%d) != vendidos do evento (%d)", somaUn, stats.TotalProductsSold)
	}
}

// TestConfirmedMetricsSplitVipCartAcrossEvents — o coração da F3: um pedido
// âncora no evento X com itens vendidos em X e em Y aparece nas métricas do
// evento CERTO. X não leva o crédito de Y, e Y aparece mesmo sem ser o âncora.
func TestConfirmedMetricsSplitVipCartAcrossEvents(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID := seedStore(t)
	eventX := seedEventInStore(t, storeID)
	eventY := seedEventInStore(t, storeID)
	sx := seedSession(t, eventX, 1)
	sy := seedSession(t, eventY, 1)

	produtoX := seedProduct(t, eventX, 3000) // vendido na live de X
	produtoY := seedProduct(t, eventY, 5000) // vendido na live de Y

	// Carrinho VIP eterno, ancorado em X, comprou nos dois eventos e pagou.
	vip, _ := getOrCreateVip(t, storeID, eventX, "alisson")
	// order.event_id = X (âncora), mas os itens vêm de sessões de X e de Y.
	sealPaidOrder(t, vip.ID, storeID, eventX, []sealItem{
		{produtoX, "Produto X", 2, 3000, sx}, // 6000 -> evento X
		{produtoY, "Produto Y", 1, 5000, sy}, // 5000 -> evento Y
	})

	statsX, err := testRepo.GetEventStats(ctx, eventX)
	if err != nil {
		t.Fatalf("GetEventStats X: %v", err)
	}
	statsY, err := testRepo.GetEventStats(ctx, eventY)
	if err != nil {
		t.Fatalf("GetEventStats Y: %v", err)
	}

	// X leva só a parte de X; Y leva só a parte de Y — apesar do âncora ser X.
	if statsX.ConfirmedRevenue != 6000 || statsX.TotalProductsSold != 2 {
		t.Errorf("evento X = %d cents / %d un, quero 6000/2 (só a parte de X)",
			statsX.ConfirmedRevenue, statsX.TotalProductsSold)
	}
	if statsY.ConfirmedRevenue != 5000 || statsY.TotalProductsSold != 1 {
		t.Errorf("evento Y = %d cents / %d un, quero 5000/1 (Y recebe crédito mesmo sem ser âncora)",
			statsY.ConfirmedRevenue, statsY.TotalProductsSold)
	}

	// Top Produtos: cada produto no evento onde foi vendido, e só nele.
	prodsX, err := testRepo.ListProductsByEvent(ctx, eventX)
	if err != nil {
		t.Fatalf("ListProductsByEvent X: %v", err)
	}
	prodsY, err := testRepo.ListProductsByEvent(ctx, eventY)
	if err != nil {
		t.Fatalf("ListProductsByEvent Y: %v", err)
	}
	assertOnlyProduct := func(evName string, rows []EventProductRow, wantID string, wantQty int) {
		if len(rows) != 1 {
			t.Errorf("%s: %d produtos, quero 1 (%+v)", evName, len(rows), rows)
			return
		}
		if rows[0].ID != wantID || rows[0].TotalQuantity != wantQty {
			t.Errorf("%s: produto %s q=%d, quero %s q=%d", evName, rows[0].ID, rows[0].TotalQuantity, wantID, wantQty)
		}
	}
	assertOnlyProduct("evento X", prodsX, produtoX, 2)
	assertOnlyProduct("evento Y", prodsY, produtoY, 1)

	// Quebra por transmissão: a sessão de Y aparece na métrica de Y.
	confY, err := testRepo.ListSessionConfirmedRevenueByEvent(ctx, eventY)
	if err != nil {
		t.Fatalf("ListSessionConfirmedRevenueByEvent Y: %v", err)
	}
	var revY int64
	sawSy := false
	for _, c := range confY {
		revY += c.RevenueCents
		if c.SessionID == sy {
			sawSy = true
		}
	}
	if !sawSy {
		t.Errorf("sessão de Y (%s) ausente na quebra confirmada do evento Y: %+v", sy, confY)
	}
	if revY != statsY.ConfirmedRevenue {
		t.Errorf("soma das sessões de Y (%d) != confirmado de Y (%d)", revY, statsY.ConfirmedRevenue)
	}

	// E a métrica por sessão do serviço (Fatia 5) fecha nos dois eventos.
	svc := NewService(testRepo, zap.NewNop())
	mY, err := svc.GetSessionMetrics(ctx, eventY, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics Y: %v", err)
	}
	if mY.ConfirmedRevenue != 5000 {
		t.Errorf("GetSessionMetrics(Y).ConfirmedRevenue = %d, quero 5000", mY.ConfirmedRevenue)
	}
}

package live

// Clientes VIP / F3 — atribuição do PROJETADO (carrinho aberto) por evento de
// ORIGEM. O requisito é "adicionou produto no evento Y -> métrica do evento Y",
// e isso vale já com o carrinho ABERTO (antes do pagamento), não só na venda.
//
// Um carrinho VIP eterno ancorado no evento X, ainda aberto, com itens
// adicionados em X e em Y: a expectativa de venda de X é só a fatia de X, e a de
// Y é a fatia de Y — mesmo o carrinho estando ancorado em X. Carrinho normal de
// um evento continua idêntico (as queries ampliadas não pegam carrinho não-VIP
// de outro evento).

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestProjectedMetricsSplitOpenVipCartAcrossEvents(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID := seedStore(t)
	eventX := seedEventInStore(t, storeID)
	eventY := seedEventInStore(t, storeID)
	sx := seedSession(t, eventX, 1)
	sy := seedSession(t, eventY, 1)
	produtoX := seedProduct(t, eventX, 3000)
	produtoY := seedProduct(t, eventY, 5000)

	// Carrinho VIP eterno, ancorado em X, ABERTO (não pago). Adiciona 2 do
	// produtoX na live de X e 1 do produtoY na live de Y — mesmo carrinho.
	vip, _ := getOrCreateVip(t, storeID, eventX, "alisson")
	add := func(session, product string, qty int, price int64) {
		t.Helper()
		if err := testRepo.AddCartItem(ctx, AddCartItemParams{
			CartID: vip.ID, ProductID: product, SessionID: session, Quantity: qty, UnitPrice: price,
		}); err != nil {
			t.Fatalf("AddCartItem: %v", err)
		}
	}
	add(sx, produtoX, 2, 3000) // 6000 -> evento X
	add(sy, produtoY, 1, 5000) // 5000 -> evento Y

	statsX, err := testRepo.GetEventStats(ctx, eventX)
	if err != nil {
		t.Fatalf("GetEventStats X: %v", err)
	}
	statsY, err := testRepo.GetEventStats(ctx, eventY)
	if err != nil {
		t.Fatalf("GetEventStats Y: %v", err)
	}
	// Wait: GetEventStats repo returns the SQL row (projected still anchor). O
	// override por origem mora no SERVIÇO. Portanto a prova por evento usa o
	// serviço; aqui só checamos open_carts (SQL ampliado).
	if statsX.OpenCarts != 1 {
		t.Errorf("open_carts X = %d, quero 1", statsX.OpenCarts)
	}
	if statsY.OpenCarts != 1 {
		t.Errorf("open_carts Y = %d, quero 1 (carrinho VIP ancorado em X toca Y)", statsY.OpenCarts)
	}

	svc := NewService(testRepo, zap.NewNop())
	outX, err := svc.GetEventStats(ctx, eventX, storeID)
	if err != nil {
		t.Fatalf("svc.GetEventStats X: %v", err)
	}
	outY, err := svc.GetEventStats(ctx, eventY, storeID)
	if err != nil {
		t.Fatalf("svc.GetEventStats Y: %v", err)
	}
	if outX.ProjectedRevenue != 6000 {
		t.Errorf("projetado X = %d, quero 6000 (só a fatia de X)", outX.ProjectedRevenue)
	}
	if outY.ProjectedRevenue != 5000 {
		t.Errorf("projetado Y = %d, quero 5000 (fatia de Y, mesmo ancorado em X)", outY.ProjectedRevenue)
	}

	// Quebra por transmissão: a sessão de Y aparece na projeção de Y e fecha
	// com o total projetado de Y (invariante da Fatia 5, agora por origem).
	mY, err := svc.GetSessionMetrics(ctx, eventY, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics Y: %v", err)
	}
	if mY.ProjectedRevenue != 5000 {
		t.Errorf("GetSessionMetrics(Y).ProjectedRevenue = %d, quero 5000", mY.ProjectedRevenue)
	}
	if mY.ProjectedRevenue != outY.ProjectedRevenue {
		t.Errorf("invariante: soma das sessões de Y (%d) != projetado do evento Y (%d)",
			mY.ProjectedRevenue, outY.ProjectedRevenue)
	}
	sawSy := false
	for _, sess := range mY.Sessions {
		if sess.SessionID == sy && sess.ProjectedRevenue == 5000 {
			sawSy = true
		}
	}
	if !sawSy {
		t.Errorf("sessão de Y (%s) não trouxe os 5000 projetados: %+v", sy, mY.Sessions)
	}

	// E o evento X: a fatia de Y não pode vazar para a projeção de X.
	mX, err := svc.GetSessionMetrics(ctx, eventX, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics X: %v", err)
	}
	if mX.ProjectedRevenue != 6000 {
		t.Errorf("GetSessionMetrics(X).ProjectedRevenue = %d, quero 6000 (Y não vaza)", mX.ProjectedRevenue)
	}
}

package live

// O card "Faturamento" de /events (GET /lives/stats).
//
// Ele somava TODO cart_item de TODO carrinho da loja, sem filtro de pagamento —
// e o comentário em cima da query afirmava estar "in sync" com o dashboard, que
// já tinha migrado para orders. O lojista via dois números diferentes para a
// mesma loja e nenhum sinal de que um deles estava errado.

import (
	"context"
	"testing"
	"time"
)

func TestStoreStatsRevenueCountsOnlyPaidOrders(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	sessionID := seedSession(t, eventID, 1)
	produto := seedProduct(t, eventID, 2500)

	// Carrinho ABERTO: 4 × 2500 = 10000 em mercadoria que ninguém pagou.
	aberto, _ := getOrCreate(t, eventID, "maria")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: aberto.ID, ProductID: produto, SessionID: sessionID, Quantity: 4, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}

	stats, err := testRepo.GetStats(ctx, storeID)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalRevenue != 0 {
		t.Errorf("faturamento = %d com zero pedido pago, quero 0", stats.TotalRevenue)
	}

	// Agora uma venda de verdade.
	paga, _ := getOrCreate(t, eventID, "joana")
	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID: paga.ID, ProductID: produto, SessionID: sessionID, Quantity: 2, UnitPrice: 2500,
	}); err != nil {
		t.Fatalf("AddCartItem: %v", err)
	}
	const total int64 = 5000
	if _, err := testPool.Exec(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status,
		   total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at)
		 VALUES ($1::uuid, $2, $3::uuid, $4::uuid, 'paid', $5, 0, 0, $5, now())`,
		paga.ID, time.Now().UnixNano()%90000+1000, storeID, eventID, total,
	); err != nil {
		t.Fatalf("selar pedido: %v", err)
	}

	stats, err = testRepo.GetStats(ctx, storeID)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalRevenue != total {
		t.Errorf("faturamento = %d, quero %d (só o pedido pago)", stats.TotalRevenue, total)
	}
	// 15000 seria a soma de tudo que passou por um carrinho.
	if stats.TotalRevenue == 15000 {
		t.Errorf("carrinho aberto voltou a entrar no faturamento: %d", stats.TotalRevenue)
	}
}

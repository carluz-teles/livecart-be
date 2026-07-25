package order_test

// Gate de cobertura — Fatia 7.
//
// Invariantes:
//   G1 Pedidos (repo.List) == carts COM order (paid+)
//   G2 Carrinhos (cart.repo.List) == carts SEM order
//   G3 Pedidos ∪ Carrinhos == todos os carts (nada some, nada duplica)
//   G4 total_amount dos Pedidos == orders.total_cents (NÃO re-soma cart_items)

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	cartpkg "livecart/apps/api/internal/cart"
	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/query"
)

// ─── helpers ────────────────────────────────────────────────────────────────

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// seedIsolatedStore cria loja + evento isolados para os testes G*.
func seedIsolatedStore(t *testing.T, tag string) (storeID, eventID string) {
	t.Helper()
	ctx := context.Background()
	n := randomSuffix()

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1, $2) RETURNING id::text`,
		tag+" Store", tag+"-"+n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seedIsolatedStore store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', $2) RETURNING id::text`,
		storeID, tag+" Ev",
	).Scan(&eventID); err != nil {
		t.Fatalf("seedIsolatedStore event: %v", err)
	}
	return
}

// seedProduct cria um produto na loja. Keyword é CHAR(4): usa 4 dígitos (1000-8999).
func seedProduct(t *testing.T, storeID string, price int64) string {
	t.Helper()
	ctx := context.Background()
	n := randomSuffix()
	kw := fmt.Sprintf("%d", time.Now().UnixNano()%8000+1000) // 4-digit keyword
	var id string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'P-'||$2, 'none', 'e-'||$2, $3, $4, 100) RETURNING id::text`,
		storeID, n, kw, price,
	).Scan(&id); err != nil {
		t.Fatalf("seedProduct: %v", err)
	}
	return id
}

// insertCart inserts a raw cart and returns its id.
func insertCart(t *testing.T, eventID, handle, token string, shortID int, paymentStatus string, paidAt *time.Time) string {
	t.Helper()
	ctx := context.Background()
	n := randomSuffix()
	var id string
	var err error
	if paidAt != nil {
		err = testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
			   status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents)
			 VALUES ($1, 'u-'||$2, $3, $4, $5,
			   'checkout', $6, $7, 0, 0) RETURNING id::text`,
			eventID, n, handle, token, shortID, paymentStatus, *paidAt,
		).Scan(&id)
	} else {
		err = testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
			   status, payment_status, coupon_discount_cents, shipping_cost_cents)
			 VALUES ($1, 'u-'||$2, $3, $4, $5, 'active', $6, 0, 0) RETURNING id::text`,
			eventID, n, handle, token, shortID, paymentStatus,
		).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insertCart: %v", err)
	}
	return id
}

// addItem inserts a cart_item.
func addItem(t *testing.T, cartID, productID string, qty int, price int64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, $4, 0)`, cartID, productID, qty, price,
	); err != nil {
		t.Fatalf("addItem: %v", err)
	}
}

// ─── G1/G2/G3/G4 ────────────────────────────────────────────────────────────

func TestFatia7_UnionCoversAllCarts(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	storeID, eventID := seedIsolatedStore(t, "g3")
	prodID := seedProduct(t, storeID, 5000)
	prodID2 := seedProduct(t, storeID, 3000)

	// C1: cart pago → order materializada
	now := time.Now()
	paidCartID := insertCart(t, eventID, "@g3paid", "tok-g3p", 77101, "paid", &now)
	addItem(t, paidCartID, prodID, 2, 5000)
	if err := l.OnCartPaid(ctx, paidCartID, storeID, 10000, nil); err != nil {
		t.Fatalf("C1 OnCartPaid: %v", err)
	}

	// C2: cart não-pago (ativo, sem order)
	unpaidCartID := insertCart(t, eventID, "@g3pending", "tok-g3u", 77102, "pending", nil)
	addItem(t, unpaidCartID, prodID, 1, 5000)

	// C3: cart pago → order materializada → simulação de estorno
	refCartID := insertCart(t, eventID, "@g3ref", "tok-g3r", 77103, "paid", &now)
	addItem(t, refCartID, prodID2, 1, 3000)
	if err := l.OnCartPaid(ctx, refCartID, storeID, 3000, nil); err != nil {
		t.Fatalf("C3 OnCartPaid: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET payment_status = 'refunded' WHERE id = $1`, refCartID,
	); err != nil {
		t.Fatalf("C3 refund: %v", err)
	}

	// ─── G1: Pedidos contém apenas carts com order ────────────────────────────

	orderRepo := order.NewRepository(testPool)
	orderResult, err := orderRepo.List(ctx, order.ListOrdersParams{
		StoreID:    storeID,
		Pagination: query.Pagination{Page: 1, Limit: 100},
		Sorting:    query.Sorting{SortBy: "created_at", SortOrder: "desc"},
	})
	if err != nil {
		t.Fatalf("G1 order list: %v", err)
	}

	inOrders := make(map[string]bool)
	for _, o := range orderResult.Orders {
		inOrders[o.ID] = true
	}

	if !inOrders[paidCartID] {
		t.Errorf("G1: paid cart %s deve aparecer em Pedidos", paidCartID)
	}
	if !inOrders[refCartID] {
		t.Errorf("G1: cart estornado %s deve aparecer em Pedidos (order existe)", refCartID)
	}
	if inOrders[unpaidCartID] {
		t.Errorf("G1: cart não-pago %s NÃO deve aparecer em Pedidos", unpaidCartID)
	}

	// ─── G2: Carrinhos contém apenas carts sem order ──────────────────────────

	cartRepo := cartpkg.NewRepository(testPool)
	cartResult, err := cartRepo.List(ctx, cartpkg.ListCartsParams{
		StoreID:    storeID,
		Pagination: query.Pagination{Page: 1, Limit: 100},
		Sorting:    query.Sorting{SortBy: "created_at", SortOrder: "desc"},
	})
	if err != nil {
		t.Fatalf("G2 cart list: %v", err)
	}

	inCarts := make(map[string]bool)
	for _, c := range cartResult.Carts {
		inCarts[c.ID] = true
	}

	if !inCarts[unpaidCartID] {
		t.Errorf("G2: cart não-pago %s deve aparecer em Carrinhos", unpaidCartID)
	}
	if inCarts[paidCartID] {
		t.Errorf("G2: cart pago %s NÃO deve aparecer em Carrinhos", paidCartID)
	}
	if inCarts[refCartID] {
		t.Errorf("G2: cart estornado %s NÃO deve aparecer em Carrinhos (tem order)", refCartID)
	}

	// ─── G3: Pedidos ∪ Carrinhos == todos os carts desta store ────────────────

	rows, err := testPool.Query(ctx,
		`SELECT c.id::text FROM carts c
		 JOIN live_events e ON e.id = c.event_id
		 WHERE e.store_id = $1`, storeID)
	if err != nil {
		t.Fatalf("G3 all carts: %v", err)
	}
	defer rows.Close()

	union := make(map[string]bool)
	for id := range inOrders {
		union[id] = true
	}
	for id := range inCarts {
		if union[id] {
			t.Errorf("G3: cart %s aparece em AMBOS Pedidos e Carrinhos", id)
		}
		union[id] = true
	}

	for rows.Next() {
		var id string
		rows.Scan(&id)
		if !union[id] {
			t.Errorf("G3: cart %s sumiu — não está em Pedidos nem em Carrinhos", id)
		}
	}

	// ─── G4: total_amount nos Pedidos == orders.total_cents ──────────────────

	for _, o := range orderResult.Orders {
		var orderTotalCents int64
		if err := testPool.QueryRow(ctx,
			`SELECT COALESCE(total_cents, 0) FROM orders WHERE cart_id = $1`, o.ID,
		).Scan(&orderTotalCents); err != nil {
			t.Fatalf("G4 query: %v", err)
		}
		if o.TotalAmount != orderTotalCents {
			t.Errorf("G4: cart %s total_amount=%d != orders.total_cents=%d",
				o.ID, o.TotalAmount, orderTotalCents)
		}
	}
}

// ─── CartStats ───────────────────────────────────────────────────────────────

func TestFatia7_CartStats(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID := seedIsolatedStore(t, "stats7")
	prodID := seedProduct(t, storeID, 2000)

	// Cart ativo com handle (recuperável)
	now := time.Now().Add(1 * time.Hour) // expires_at no futuro
	var c1 string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, expires_at)
		 VALUES ($1, 'u-s1', '@handle1', 'tok-st1', 88101, 'active', 'pending', $2) RETURNING id::text`,
		eventID, now,
	).Scan(&c1); err != nil {
		t.Fatalf("cart1: %v", err)
	}
	addItem(t, c1, prodID, 3, 2000) // 6000

	// Cart ativo sem handle (não-recuperável)
	var c2 string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, expires_at)
		 VALUES ($1, '', '', 'tok-st2', 88102, 'active', 'pending', $2) RETURNING id::text`,
		eventID, now,
	).Scan(&c2); err != nil {
		t.Fatalf("cart2: %v", err)
	}
	addItem(t, c2, prodID, 1, 2000) // 2000

	// Cart expirado (não entra em open_carts mas entra no total de Carrinhos)
	exp := time.Now().Add(-1 * time.Hour)
	var c3 string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, expires_at)
		 VALUES ($1, 'u-s3', '@exp3', 'tok-st3', 88103, 'expired', 'pending', $2) RETURNING id::text`,
		eventID, exp,
	).Scan(&c3); err != nil {
		t.Fatalf("cart3: %v", err)
	}

	cartRepo := cartpkg.NewRepository(testPool)
	cartSvc := cartpkg.NewService(cartRepo, zap.NewNop())
	stats, err := cartSvc.GetStats(ctx, storeID)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	if stats.OpenCarts != 2 {
		t.Errorf("open_carts want 2, got %d", stats.OpenCarts)
	}
	if stats.OpenValueCents != 6000+2000 {
		t.Errorf("open_value_cents want 8000, got %d", stats.OpenValueCents)
	}
	if stats.AvgTicketCents != 4000 {
		t.Errorf("avg_ticket_cents want 4000, got %d", stats.AvgTicketCents)
	}
	if stats.RecoverableCarts != 1 {
		t.Errorf("recoverable_carts want 1, got %d", stats.RecoverableCarts)
	}
}

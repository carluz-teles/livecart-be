package billing

// TestGMVCanonical — AC novo do WS5.
// Semeia carts com múltiplos itens e prova que TODOS os read-models refatorados
// retornam o MESMO valor que a fórmula golden SUM(quantity*unit_price).
// Deve falhar se qualquer site divergir da função canônica cart_product_total_cents.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// seedEventAndCarts cria um evento com N carts pagos em uma loja e devolve
// (eventID, sessionID, []cartID, goldenTotals) onde goldenTotals[i] é o
// SUM(quantity*unit_price) real do cart i.
func seedEventAndCarts(t *testing.T, storeID string) (eventID, sessionID string, cartIDs []string, goldens []int64) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UnixNano() // int64, cabe em bigint

	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'GMV Canonical Test') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seedEventAndCarts event: %v", err)
	}

	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status) VALUES ($1, 'ended') RETURNING id::text`,
		eventID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("seedEventAndCarts session: %v", err)
	}

	// Cart A: 3 itens, qty×price: 1×1000, 2×2500, 1×750 → 1000+5000+750 = 6750
	// Cart B: 2 itens, qty×price: 1×5000, 3×300       → 5000+900 = 5900
	type cartSpec struct {
		items [][2]int64 // {quantity, unit_price}
	}
	specs := []cartSpec{
		{items: [][2]int64{{1, 1000}, {2, 2500}, {1, 750}}},
		{items: [][2]int64{{1, 5000}, {3, 300}}},
	}

	for i, spec := range specs {
		// Usa base+i para garantir unicidade sem ultrapassar o limite do bigint.
		uid := base + int64(i)
		shortID := uid % 100000
		var cartID string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token, short_id, status, payment_status, paid_at)
			 VALUES ($1, $2, 'u-gmv-'||$3, '@gmv'||$3, 'tok-gmv-'||$3, $4, 'checkout', 'paid', now())
			 RETURNING id::text`,
			eventID, sessionID, fmt.Sprintf("%d", uid), shortID,
		).Scan(&cartID); err != nil {
			t.Fatalf("seedEventAndCarts cart %d: %v", i, err)
		}

		var golden int64
		for j, item := range spec.items {
			qty, price := item[0], item[1]
			extID := fmt.Sprintf("gmv-%d-%d-%d", base, i, j)
			// keyword é character(4): usa offset dentro de [1000,9999]
			kw := fmt.Sprintf("%04d", (base+int64(i*10+j))%9000+1000)
			var productID string
			if err := testPool.QueryRow(ctx,
				`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
				 VALUES ($1, 'ProdGMV', 'none', $2, $3, $4, 100) RETURNING id::text`,
				storeID, extID, kw, price,
			).Scan(&productID); err != nil {
				t.Fatalf("seedEventAndCarts product [%d,%d]: %v", i, j, err)
			}
			if _, err := testPool.Exec(ctx,
				`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
				 VALUES ($1, $2, $3, $4, 0)`,
				cartID, productID, qty, price,
			); err != nil {
				t.Fatalf("seedEventAndCarts cart_item [%d,%d]: %v", i, j, err)
			}
			golden += qty * price
		}

		// Fatia 4: revenue queries agora lêem de orders; seeia order selada para cada cart pago.
		shortOrderID := (uid + int64(i) + 500) % 90000
		if _, err := testPool.Exec(ctx,
			`INSERT INTO orders (cart_id, short_id, store_id, event_id, status,
			   total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at)
			 VALUES ($1, $2, $3, $4, 'paid', $5, 0, 0, $5, now())`,
			cartID, shortOrderID, storeID, eventID, golden,
		); err != nil {
			t.Fatalf("seedEventAndCarts order %d: %v", i, err)
		}

		cartIDs = append(cartIDs, cartID)
		goldens = append(goldens, golden)
	}
	return
}

func mustParseUUIDStr(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	id, err := parseUUID(s)
	if err != nil {
		t.Fatalf("mustParseUUIDStr(%q): %v", s, err)
	}
	return id
}

// goldenSum computa SUM(quantity*unit_price) inline no banco para um cart.
func goldenSum(t *testing.T, cartID string) int64 {
	t.Helper()
	var v int64
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(quantity * unit_price), 0)::bigint FROM cart_items WHERE cart_id = $1::uuid`,
		cartID,
	).Scan(&v); err != nil {
		t.Fatalf("goldenSum(%s): %v", cartID, err)
	}
	return v
}

// TestGMVCanonical_GetCartGMVCents verifica que GetCartGMVCents (delegando a
// cart_product_total_cents) retorna o mesmo valor que a fórmula inline.
func TestGMVCanonical_GetCartGMVCents(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	_, _, cartIDs, goldens := seedEventAndCarts(t, storeID)

	ctx := context.Background()
	for i, cartID := range cartIDs {
		got, err := testQueries.GetCartGMVCents(ctx, mustParseUUIDStr(t, cartID))
		if err != nil {
			t.Fatalf("cart[%d] GetCartGMVCents: %v", i, err)
		}
		if got != goldens[i] {
			t.Errorf("cart[%d] GetCartGMVCents: got %d, want %d (golden inline SUM)", i, got, goldens[i])
		}
	}
}

// TestGMVCanonical_GetCartTotals verifica total_value de GetCartTotals.
func TestGMVCanonical_GetCartTotals(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	_, _, cartIDs, goldens := seedEventAndCarts(t, storeID)

	ctx := context.Background()
	for i, cartID := range cartIDs {
		row, err := testQueries.GetCartTotals(ctx, mustParseUUIDStr(t, cartID))
		if err != nil {
			t.Fatalf("cart[%d] GetCartTotals: %v", i, err)
		}
		if row.TotalValue != goldens[i] {
			t.Errorf("cart[%d] GetCartTotals.TotalValue: got %d, want %d", i, row.TotalValue, goldens[i])
		}
	}
}

// TestGMVCanonical_EventStats verifica confirmed_revenue de GetEventStats.
func TestGMVCanonical_EventStats(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	eventID, _, cartIDs, goldens := seedEventAndCarts(t, storeID)

	// expected = soma dos goldens (todos os carts estão pagos)
	var expected int64
	for _, g := range goldens {
		expected += g
	}
	// verifica também vs banco direto
	var dbSum int64
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(ci.quantity * ci.unit_price),0)::bigint
		 FROM carts c JOIN cart_items ci ON ci.cart_id = c.id
		 WHERE c.event_id = $1::uuid AND c.payment_status = 'paid'`, eventID,
	).Scan(&dbSum); err != nil {
		t.Fatalf("dbSum event: %v", err)
	}
	if expected != dbSum {
		t.Fatalf("setup inconsistente: expected=%d dbSum=%d", expected, dbSum)
	}

	row, err := testQueries.GetEventStats(context.Background(), mustParseUUIDStr(t, eventID))
	if err != nil {
		t.Fatalf("GetEventStats: %v", err)
	}
	if row.ConfirmedRevenue != expected {
		t.Errorf("GetEventStats.ConfirmedRevenue: got %d, want %d", row.ConfirmedRevenue, expected)
	}

	_ = cartIDs
}

// TestGMVCanonical_SessionStats verifica paid_revenue de GetSessionStats.
func TestGMVCanonical_SessionStats(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	_, sessionID, cartIDs, goldens := seedEventAndCarts(t, storeID)

	var expected int64
	for _, g := range goldens {
		expected += g
	}

	row, err := testQueries.GetSessionStats(context.Background(), mustParseUUIDStr(t, sessionID))
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	if row.PaidRevenue != expected {
		t.Errorf("GetSessionStats.PaidRevenue: got %d, want %d", row.PaidRevenue, expected)
	}

	_ = cartIDs
}

// TestGMVCanonical_DashboardStats verifica total_revenue de GetDashboardStats.
func TestGMVCanonical_DashboardStats(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	_, _, cartIDs, goldens := seedEventAndCarts(t, storeID)

	var expected int64
	for _, g := range goldens {
		expected += g
	}

	row, err := testQueries.GetDashboardStats(context.Background(), mustParseUUIDStr(t, storeID))
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	if row.TotalRevenue != expected {
		t.Errorf("GetDashboardStats.TotalRevenue: got %d, want %d", row.TotalRevenue, expected)
	}

	_ = cartIDs
}

// TestGMVCanonical_GoldenCrossCheck verifica para cada cart que goldenSum inline
// == GetCartGMVCents, bloqueando divergências futuras entre a função e os dados.
func TestGMVCanonical_GoldenCrossCheck(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	_, _, cartIDs, _ := seedEventAndCarts(t, storeID)

	ctx := context.Background()
	for i, cartID := range cartIDs {
		inline := goldenSum(t, cartID)
		got, err := testQueries.GetCartGMVCents(ctx, mustParseUUIDStr(t, cartID))
		if err != nil {
			t.Fatalf("cart[%d] GetCartGMVCents: %v", i, err)
		}
		if got != inline {
			t.Errorf("cart[%d] DIVERGÊNCIA: cart_product_total_cents=%d, inline SUM=%d", i, got, inline)
		}
	}
}

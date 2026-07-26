package dashboard_test

// Golden de paridade para Fatia 5 — cutover de leitura por-produto via order_items.
//
// Grupo B (correção consciente de bug): as queries somavam carts não-pagos.
// Agora lêem de order_items de orders com status='paid'.
//
// Queries cobertas:
//   GetTopProducts (dashboard.sql)      — escopado por store_id
//   ListProductsByEvent (cart.sql)      — escopado por event_id
//
// Invariantes:
//   P1  GetTopProducts retorna apenas produtos de orders pagas
//   P2  GetTopProducts difere do antigo (all-carts) quando há carts não-pagos
//   P3  ListProductsByEvent retorna apenas produtos de orders pagas
//   P4  ListProductsByEvent difere do antigo (all-carts) quando há carts não-pagos
//   P5  Orders com status != 'paid' (ex: 'pending_payment') NÃO são contadas

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ─── Seed para Fatia 5 ────────────────────────────────────────────────────────

type f5Seed struct {
	storeID string
	eventID string

	prodAID string // produto A: aparece em paid order + unpaid cart
	prodBID string // produto B: aparece apenas em paid order

	// Valores pagos reais (order_items)
	paidQtyA int32
	paidQtyB int32
	paidRevA int64
	paidRevB int64

	// Valores extras de carts não-pagos (para provar divergência)
	unpaidExtraQtyA int32
	unpaidExtraRevA int64
}

// seedF5 monta dataset multi-produto com bordas:
//
//	Paid order  → prodA (3×10000=30000) + prodB (2×5000=10000)  → order_items populados
//	Unpaid cart → prodA (5×10000=50000)                          → só cart_items, sem order
//	Pending order → prodA (1×10000=10000)                        → order status=pending_payment
func seedF5(t *testing.T) f5Seed {
	t.Helper()
	ctx := context.Background()
	nRaw := time.Now().UnixNano()
	n := fmt.Sprintf("%d", nRaw)

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('F5 Store', 'f5-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'F5 Event') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	kwA := fmt.Sprintf("%04d", nRaw%9000+1000)
	var prodAID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'F5ProdA', 'none', 'f5a-'||$2, $3, 10000, 100) RETURNING id::text`,
		storeID, n, kwA,
	).Scan(&prodAID); err != nil {
		t.Fatalf("seed prodA: %v", err)
	}

	kwB := fmt.Sprintf("%04d", (nRaw+1)%9000+1000)
	var prodBID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'F5ProdB', 'none', 'f5b-'||$2, $3, 5000, 100) RETURNING id::text`,
		storeID, n, kwB,
	).Scan(&prodBID); err != nil {
		t.Fatalf("seed prodB: %v", err)
	}

	// ── Cart pago (para cart_items do antigo) ──────────────────────────────────
	var paidCartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents)
		 VALUES ($1, 'f5p-'||$2, '@f5paid', 'f5tok-p-'||$2,
		   ($2::bigint%90000)+1000, 'checkout', 'paid', now(), 0, 0) RETURNING id::text`,
		eventID, n,
	).Scan(&paidCartID); err != nil {
		t.Fatalf("seed paid cart: %v", err)
	}
	// cart_items do cart pago (baseline "antigo")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 3, 10000, 0), ($1, $3, 2, 5000, 0)`,
		paidCartID, prodAID, prodBID,
	); err != nil {
		t.Fatalf("seed paid cart_items: %v", err)
	}

	// Order selada (paid) para o cart pago
	var orderID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status,
		   total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at)
		 VALUES ($1, ($2::bigint%90000)+10000, $3, $4, 'paid', 40000, 0, 0, 40000, now())
		 RETURNING id::text`,
		paidCartID, n, storeID, eventID,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// order_items: fonte correta para a nova query
	if _, err := testPool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price)
		 VALUES ($1, $2, 'F5ProdA', 3, 10000),
		        ($1, $3, 'F5ProdB', 2, 5000)`,
		orderID, prodAID, prodBID,
	); err != nil {
		t.Fatalf("seed order_items: %v", err)
	}

	// ── Cart NÃO-pago: prodA extra → diverge do antigo ────────────────────────
	var unpaidCartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, coupon_discount_cents)
		 VALUES ($1, 'f5u-'||$2, '@f5unpaid', 'f5tok-u-'||$2,
		   ($2::bigint%90000)+50000, 'checkout', 'pending', 0) RETURNING id::text`,
		eventID, n,
	).Scan(&unpaidCartID); err != nil {
		t.Fatalf("seed unpaid cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 5, 10000, 0)`,
		unpaidCartID, prodAID,
	); err != nil {
		t.Fatalf("seed unpaid cart_items: %v", err)
	}

	// ── Order com status pending_payment (sem order_items) ────────────────────
	var pendingCartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, coupon_discount_cents)
		 VALUES ($1, 'f5pend-'||$2, '@f5pend', 'f5tok-pend-'||$2,
		   ($2::bigint%90000)+70000, 'checkout', 'pending', 0) RETURNING id::text`,
		eventID, n,
	).Scan(&pendingCartID); err != nil {
		t.Fatalf("seed pending cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 1, 10000, 0)`,
		pendingCartID, prodAID,
	); err != nil {
		t.Fatalf("seed pending cart_items: %v", err)
	}
	var pendingOrderID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status,
		   total_cents, discount_cents, shipping_cents)
		 VALUES ($1, ($2::bigint%90000)+80000, $3, $4, 'pending_payment', 10000, 0, 0)
		 RETURNING id::text`,
		pendingCartID, n, storeID, eventID,
	).Scan(&pendingOrderID); err != nil {
		t.Fatalf("seed pending order: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price)
		 VALUES ($1, $2, 'F5ProdA', 1, 10000)`,
		pendingOrderID, prodAID,
	); err != nil {
		t.Fatalf("seed pending order_items: %v", err)
	}

	return f5Seed{
		storeID:         storeID,
		eventID:         eventID,
		prodAID:         prodAID,
		prodBID:         prodBID,
		paidQtyA:        3,
		paidQtyB:        2,
		paidRevA:        30000,
		paidRevB:        10000,
		unpaidExtraQtyA: 5,
		unpaidExtraRevA: 50000,
	}
}

func parseUUIDf5(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parseUUID(%q): %v", s, err)
	}
	return u
}

// ─── P1/P2: GetTopProducts ────────────────────────────────────────────────────

func TestProductRevenue_GetTopProducts_PaidOnly(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)

	rows, err := testQueries.GetTopProducts(ctx, parseUUIDf5(t, seed.storeID))
	if err != nil {
		t.Fatalf("GetTopProducts: %v", err)
	}

	byProduct := map[string]struct {
		qty int32
		rev int64
	}{}
	for _, r := range rows {
		id := r.ID.String()
		byProduct[id] = struct {
			qty int32
			rev int64
		}{r.TotalSold, r.TotalRevenue}
	}

	// P1: prodA e prodB aparecem com valores paid-only
	if got := byProduct[seed.prodAID]; got.qty != seed.paidQtyA || got.rev != seed.paidRevA {
		t.Errorf("GetTopProducts prodA: want qty=%d rev=%d, got qty=%d rev=%d",
			seed.paidQtyA, seed.paidRevA, got.qty, got.rev)
	}
	if got := byProduct[seed.prodBID]; got.qty != seed.paidQtyB || got.rev != seed.paidRevB {
		t.Errorf("GetTopProducts prodB: want qty=%d rev=%d, got qty=%d rev=%d",
			seed.paidQtyB, seed.paidRevB, got.qty, got.rev)
	}

	// P2: valor de prodA DIFERE do antigo (all-carts: 3+5+1=9, novo: 3)
	oldQtyA := seed.paidQtyA + seed.unpaidExtraQtyA + 1 // +1 do pending cart
	if got := byProduct[seed.prodAID]; got.qty == oldQtyA {
		t.Errorf("GetTopProducts prodA: got all-carts qty=%d — bug B não corrigido; quer paid-only=%d",
			oldQtyA, seed.paidQtyA)
	}

	// P5: pending_payment order NÃO soma (prodA qty continua = paidQtyA, não paidQtyA+1)
	if got := byProduct[seed.prodAID]; got.qty == seed.paidQtyA+1 {
		t.Errorf("GetTopProducts: pending_payment order está sendo contada (qty=%d)", got.qty)
	}
}

// ─── P3/P4: ListProductsByEvent ───────────────────────────────────────────────

func TestProductRevenue_ListProductsByEvent_PaidOnly(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)

	rows, err := testQueries.ListProductsByEvent(ctx, parseUUIDf5(t, seed.eventID))
	if err != nil {
		t.Fatalf("ListProductsByEvent: %v", err)
	}

	byProduct := map[string]struct {
		qty int32
		rev int64
	}{}
	for _, r := range rows {
		id := r.ID.String()
		byProduct[id] = struct {
			qty int32
			rev int64
		}{r.TotalQuantity, r.TotalRevenue}
	}

	// P3: prodA e prodB aparecem com valores paid-only
	if got := byProduct[seed.prodAID]; got.qty != seed.paidQtyA || got.rev != seed.paidRevA {
		t.Errorf("ListProductsByEvent prodA: want qty=%d rev=%d, got qty=%d rev=%d",
			seed.paidQtyA, seed.paidRevA, got.qty, got.rev)
	}
	if got := byProduct[seed.prodBID]; got.qty != seed.paidQtyB || got.rev != seed.paidRevB {
		t.Errorf("ListProductsByEvent prodB: want qty=%d rev=%d, got qty=%d rev=%d",
			seed.paidQtyB, seed.paidRevB, got.qty, got.rev)
	}

	// P4: valor de prodA DIFERE do antigo (status!='expired' pegaria carts não-pagos)
	oldQtyA := seed.paidQtyA + seed.unpaidExtraQtyA + 1
	if got := byProduct[seed.prodAID]; got.qty == oldQtyA {
		t.Errorf("ListProductsByEvent prodA: got all-carts qty=%d — bug B não corrigido; quer paid-only=%d",
			oldQtyA, seed.paidQtyA)
	}

	// P5: pending_payment order NÃO soma
	if got := byProduct[seed.prodAID]; got.qty == seed.paidQtyA+1 {
		t.Errorf("ListProductsByEvent: pending_payment order está sendo contada (qty=%d)", got.qty)
	}
}

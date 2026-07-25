package order_test

// Golden test de paridade para OnCartPaid (Fase A).
// Gated em TEST_DATABASE_URL (mesma infra do billing/testmain_test.go).
//
// Invariantes travadas:
//   P1 orders.total_cents == cart_product_total_cents(cart_id)
//   P2 SUM(order_items.quantity * unit_price) == orders.total_cents
//   P3 paid_total_cents == total_cents - discount_cents + shipping_cents
//   P4 idempotência: 2ª chamada não cria nova Order nem duplica itens

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/order/listeners"
	"livecart/apps/api/lib/database"
	"go.uber.org/zap"
)

// ─── Test harness ────────────────────────────────────────────────────────────

var (
	testPool    *pgxpool.Pool
	testQueries *sqlc.Queries
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		return m.Run()
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TEST_DATABASE_URL inválida: %v\n", err)
		return 1
	}
	defer admin.Close()

	dbName := fmt.Sprintf("lc_order_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		fmt.Fprintf(os.Stderr, "criando DB de teste: %v\n", err)
		return 1
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	}()

	u, _ := url.Parse(adminURL)
	u.Path = "/" + dbName
	testURL := u.String()

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "db", "migrations")
	if err := database.RunMigrations(testURL, migrationsPath); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conectando: %v\n", err)
		return 1
	}
	defer pool.Close()

	testPool = pool
	testQueries = sqlc.New(pool)
	return m.Run()
}

func requireDB(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não setada")
	}
}

// ─── Seed helpers ────────────────────────────────────────────────────────────

type seedCartResult struct {
	storeID   string
	eventID   string
	cartID    string
	productID string
}

// seedPaidCart cria loja → evento → produto → cart pago com qty*unitPrice.
func seedPaidCart(t *testing.T, qty int32, unitPrice int64, discountCents, shippingCents int64) seedCartResult {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Loja Order Test', 'order-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seedPaidCart store: %v", err)
	}

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'Ev') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seedPaidCart event: %v", err)
	}

	kw := fmt.Sprintf("%d", time.Now().UnixNano()%8000+1000)
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Prod', 'none', 'ext-'||$2, $3, $4, 100) RETURNING id::text`,
		storeID, n, kw, unitPrice,
	).Scan(&productID); err != nil {
		t.Fatalf("seedPaidCart product: %v", err)
	}

	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents)
		 VALUES ($1, 'u-'||$2, '@b'||$2, 'tok-'||$2, ($2)::bigint % 100000,
		   'checkout', 'paid', now(), $3, $4) RETURNING id::text`,
		eventID, n, discountCents, shippingCents,
	).Scan(&cartID); err != nil {
		t.Fatalf("seedPaidCart cart: %v", err)
	}

	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, $4, 0)`,
		cartID, productID, qty, unitPrice,
	); err != nil {
		t.Fatalf("seedPaidCart cart_items: %v", err)
	}

	return seedCartResult{storeID: storeID, eventID: eventID, cartID: cartID, productID: productID}
}

func newListener(t *testing.T) *listeners.Listener {
	t.Helper()
	requireDB(t)
	return listeners.New(testPool, testQueries, zap.NewNop())
}

// ─── P1 total_cents == cart_product_total_cents ───────────────────────────────

func TestOnCartPaid_P1_TotalCentsMatchesCanonicalGMV(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 3, 10000, 0, 0) // gmv = 30000

	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 30000, nil); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	var totalCents, gmvFromDB int64
	if err := testPool.QueryRow(ctx,
		`SELECT total_cents, cart_product_total_cents(cart_id) FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&totalCents, &gmvFromDB); err != nil {
		t.Fatalf("query order: %v", err)
	}

	if totalCents != gmvFromDB {
		t.Errorf("P1: total_cents=%d != cart_product_total_cents=%d", totalCents, gmvFromDB)
	}
}

// ─── P2 SUM(order_items.qty*price) == total_cents ───────────────────────────

func TestOnCartPaid_P2_ItemsSumMatchesTotalCents(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 5, 2000, 0, 0) // gmv = 10000

	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 10000, nil); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	var totalCents, itemsSum int64
	if err := testPool.QueryRow(ctx, `
		SELECT o.total_cents,
		       COALESCE(SUM(oi.quantity * oi.unit_price), 0)
		FROM orders o
		JOIN order_items oi ON oi.order_id = o.id
		WHERE o.cart_id = $1
		GROUP BY o.total_cents`, r.cartID,
	).Scan(&totalCents, &itemsSum); err != nil {
		t.Fatalf("query items sum: %v", err)
	}

	if itemsSum != totalCents {
		t.Errorf("P2: SUM(items)=%d != total_cents=%d", itemsSum, totalCents)
	}
}

// ─── P3 paid_total_cents == total - discount + shipping ─────────────────────

func TestOnCartPaid_P3_PaidTotalFormula(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	// 2 items * 15000 = 30000 GMV, 2000 discount, 1500 shipping → paid = 29500
	r := seedPaidCart(t, 2, 15000, 2000, 1500)

	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 30000, nil); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	var total, discount, shipping, paidTotal int64
	if err := testPool.QueryRow(ctx,
		`SELECT total_cents, discount_cents, shipping_cents, paid_total_cents FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&total, &discount, &shipping, &paidTotal); err != nil {
		t.Fatalf("query order: %v", err)
	}

	want := total - discount + shipping
	if paidTotal != want {
		t.Errorf("P3: paid_total_cents=%d, want total(%d) - discount(%d) + shipping(%d) = %d",
			paidTotal, total, discount, shipping, want)
	}
}

// ─── P4 idempotência: 2ª chamada não duplica ────────────────────────────────

func TestOnCartPaid_P4_Idempotence(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 1, 5000, 0, 0)

	for i := 0; i < 3; i++ {
		if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 5000, nil); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}

	var orderCount, itemCount int
	testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&orderCount)
	testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.cart_id = $1`, r.cartID,
	).Scan(&itemCount)

	if orderCount != 1 {
		t.Errorf("P4: expected 1 order, got %d", orderCount)
	}
	if itemCount != 1 {
		t.Errorf("P4: expected 1 order_item, got %d", itemCount)
	}
}

// ─── GMV fallback: gmvCents=0 → DB fallback ──────────────────────────────────

func TestOnCartPaid_FallbackGMV(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 4, 7500, 0, 0) // gmv real = 30000

	// Passa gmvCents=0 para forçar fallback
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 0, nil); err != nil {
		t.Fatalf("OnCartPaid fallback: %v", err)
	}

	var totalCents int64
	if err := testPool.QueryRow(ctx,
		`SELECT total_cents FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&totalCents); err != nil {
		t.Fatalf("query: %v", err)
	}
	if totalCents != 30000 {
		t.Errorf("fallback: want total_cents=30000, got %d", totalCents)
	}
}

// ─── gateway_snapshot preservado ─────────────────────────────────────────────

func TestOnCartPaid_GatewaySnapshotStored(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 1, 10000, 0, 0)
	snap := json.RawMessage(`{"provider":"pagarme","status":"paid","id":"pay_xyz123"}`)

	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 10000, snap); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	var stored []byte
	if err := testPool.QueryRow(ctx,
		`SELECT gateway_snapshot FROM order_payments op
		 JOIN orders o ON o.id = op.order_id WHERE o.cart_id = $1`, r.cartID,
	).Scan(&stored); err != nil {
		t.Fatalf("query gateway_snapshot: %v", err)
	}
	// PostgreSQL reorders JSONB keys; compare semantically.
	var wantMap, gotMap map[string]any
	if err := json.Unmarshal(snap, &wantMap); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(stored, &gotMap); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	for k, v := range wantMap {
		if gotMap[k] != v {
			t.Errorf("gateway_snapshot[%q]: want %v, got %v", k, v, gotMap[k])
		}
	}
}

// ─── Backfill ────────────────────────────────────────────────────────────────

func TestBackfillFromPaidCarts(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	// Semeia 3 carts pagas sem order.
	r1 := seedPaidCart(t, 1, 1000, 0, 0)
	r2 := seedPaidCart(t, 2, 2000, 0, 0)
	r3 := seedPaidCart(t, 3, 3000, 0, 0)

	count, err := l.BackfillFromPaidCarts(ctx)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if count < 3 {
		t.Errorf("Backfill: materialised %d, want >= 3", count)
	}

	// Verifica que as 3 carts têm order agora.
	for _, cartID := range []string{r1.cartID, r2.cartID, r3.cartID} {
		var n int
		testPool.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE cart_id = $1`, cartID).Scan(&n)
		if n != 1 {
			t.Errorf("Backfill: cart %s has %d orders, want 1", cartID, n)
		}
	}

	// Segundo backfill é idempotente: count deve ser 0 (nada novo).
	count2, err := l.BackfillFromPaidCarts(ctx)
	if err != nil {
		t.Fatalf("Backfill replay: %v", err)
	}
	if count2 != 0 {
		t.Errorf("Backfill replay: expected 0 new orders, got %d", count2)
	}
}

package customer

// Golden de paridade para Fatia 4 — Grupo B (customer queries).
//
// Queries cobertas:
//   ListCustomers   (total_spent per customer)
//   GetCustomerStats (avg_spent_per_customer)
//   SearchCustomers  (total_spent per customer)
//
// Cada teste asserte:
//   NOVO == paid-only truth (30000)
//   NOVO != antigo (40000, que incluía carts não-pagos)

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/database"
)

var (
	custTestPool    *pgxpool.Pool
	custTestQueries *sqlc.Queries
)

func TestMain(m *testing.M) {
	os.Exit(custTestMain(m))
}

func custTestMain(m *testing.M) int {
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

	dbName := fmt.Sprintf("lc_f4cust_test_%d", time.Now().UnixNano())
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

	custTestPool = pool
	custTestQueries = sqlc.New(pool)
	return m.Run()
}

func requireCustDB(t *testing.T) {
	t.Helper()
	if custTestPool == nil {
		t.Skip("TEST_DATABASE_URL não setada")
	}
}

// ─── Seed helpers ─────────────────────────────────────────────────────────────

type f4CustSeed struct {
	storeID      string
	eventID      string
	customerID   string
	custHandle   string
	paidCartID   string
	unpaidCartID string
	orderID      string
	paidGMV      int64
	unpaidGMV    int64
}

// seedF4Cust cria dataset com bordas para testes de customer:
//   - 1 store / 1 evento / 1 customer
//   - Cart pago  (3 × 10000 = 30000) + Order materializada  → verdade paid-only
//   - Cart NÃO-pago (2 × 5000 = 10000, sem Order)           → expõe o bug B
func seedF4Cust(t *testing.T) f4CustSeed {
	t.Helper()
	ctx := context.Background()
	nRaw := time.Now().UnixNano()
	n := fmt.Sprintf("%d", nRaw)

	var storeID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('F4C Store', 'f4c-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var eventID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'F4C Event') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	custHandle := "@f4cust" + n
	var customerID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO customers (store_id, platform_user_id, platform_handle)
		 VALUES ($1, 'f4cu-'||$2, $3) RETURNING id::text`,
		storeID, n, custHandle,
	).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	// Produto A — para o cart pago; keyword CHAR(4): 4 dígitos em [1000,9999]
	kwA := fmt.Sprintf("%04d", nRaw%9000+1000)
	var prodAID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'F4CA', 'none', 'f4ca-'||$2, $3, 10000, 100) RETURNING id::text`,
		storeID, n, kwA,
	).Scan(&prodAID); err != nil {
		t.Fatalf("seed product A: %v", err)
	}

	// Cart 1: pago — 3 × 10000 = 30000
	const paidGMV int64 = 30000
	var paidCartID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token,
		   short_id, status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents, customer_id)
		 VALUES ($1, 'f4cpd-'||$2, '@f4cpd', 'f4ctok-p-'||$2,
		   ($2::bigint % 90000)+1000, 'checkout', 'paid', now(), 0, 0, $3) RETURNING id::text`,
		eventID, n, customerID,
	).Scan(&paidCartID); err != nil {
		t.Fatalf("seed paid cart: %v", err)
	}
	if _, err := custTestPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 3, 10000, 0)`,
		paidCartID, prodAID,
	); err != nil {
		t.Fatalf("seed paid cart items: %v", err)
	}

	// Order 1: selada para o cart pago
	var orderID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, customer_id, status,
		   total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at)
		 VALUES ($1, ($2::bigint % 90000)+10000, $3, $4, $5,
		   'paid', $6, 0, 0, $6, now()) RETURNING id::text`,
		paidCartID, n, storeID, eventID, customerID, paidGMV,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	// Produto B — para o cart NÃO-pago; keyword CHAR(4): diferente do A
	kwB := fmt.Sprintf("%04d", (nRaw+1)%9000+1000)
	var prodBID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'F4CB', 'none', 'f4cb-'||$2, $3, 5000, 100) RETURNING id::text`,
		storeID, n, kwB,
	).Scan(&prodBID); err != nil {
		t.Fatalf("seed product B: %v", err)
	}

	// Cart 2: NÃO-pago, mesmo customer
	const unpaidGMV int64 = 10000
	var unpaidCartID string
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token,
		   short_id, status, payment_status, coupon_discount_cents, customer_id)
		 VALUES ($1, 'f4cup-'||$2, '@f4cup', 'f4ctok-u-'||$2,
		   ($2::bigint % 90000)+50000, 'checkout', 'pending', 0, $3) RETURNING id::text`,
		eventID, n, customerID,
	).Scan(&unpaidCartID); err != nil {
		t.Fatalf("seed unpaid cart: %v", err)
	}
	if _, err := custTestPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 2, 5000, 0)`,
		unpaidCartID, prodBID,
	); err != nil {
		t.Fatalf("seed unpaid cart items: %v", err)
	}

	return f4CustSeed{
		storeID: storeID, eventID: eventID,
		customerID: customerID, custHandle: custHandle,
		paidCartID: paidCartID, unpaidCartID: unpaidCartID,
		orderID: orderID,
		paidGMV: paidGMV, unpaidGMV: unpaidGMV,
	}
}

func parseCustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parseUUID(%q): %v", s, err)
	}
	return u
}

// ─── Grupo B: ListCustomers.total_spent — correção do bug ────────────────────

func TestF4_GroupB_ListCustomers_TotalSpent(t *testing.T) {
	requireCustDB(t)
	ctx := context.Background()
	seed := seedF4Cust(t)

	rows, err := custTestQueries.ListCustomers(ctx, sqlc.ListCustomersParams{
		StoreID: parseCustUUID(t, seed.storeID),
		Limit:   50,
		Offset:  0,
	})
	if err != nil {
		t.Fatalf("ListCustomers: %v", err)
	}

	custUUID := parseCustUUID(t, seed.customerID)
	var got int64
	for _, r := range rows {
		if r.ID == custUUID {
			got = r.TotalSpent
		}
	}

	// Grupo B: NOVO == só-pago
	if got != seed.paidGMV {
		t.Errorf("ListCustomers total_spent: want %d (paid-only), got %d", seed.paidGMV, got)
	}

	// DIFERE do antigo (que somava todos os carts)
	antigoTotal := seed.paidGMV + seed.unpaidGMV
	if got == antigoTotal {
		t.Errorf("ListCustomers total_spent: got all-carts value (%d) — bug B não corrigido; quer apenas paid (%d)",
			antigoTotal, seed.paidGMV)
	}
}

// ─── Grupo B: GetCustomerStats.avg_spent_per_customer — correção do bug ──────

func TestF4_GroupB_GetCustomerStats_AvgSpent(t *testing.T) {
	requireCustDB(t)
	ctx := context.Background()
	seed := seedF4Cust(t)

	row, err := custTestQueries.GetCustomerStats(ctx, parseCustUUID(t, seed.storeID))
	if err != nil {
		t.Fatalf("GetCustomerStats: %v", err)
	}

	// Grupo B: NOVO avg = paidGMV / 1 customer = 30000
	if row.AvgSpentPerCustomer != seed.paidGMV {
		t.Errorf("GetCustomerStats avg_spent_per_customer: want %d (paid-only / 1 cust), got %d",
			seed.paidGMV, row.AvgSpentPerCustomer)
	}

	// DIFERE do antigo (que dividia paid+unpaid / n_customers)
	antigoAvg := seed.paidGMV + seed.unpaidGMV // = 40000 com 1 customer
	if row.AvgSpentPerCustomer == antigoAvg {
		t.Errorf("GetCustomerStats avg_spent_per_customer: got all-carts avg (%d) — bug B não corrigido; quer apenas paid (%d)",
			antigoAvg, seed.paidGMV)
	}
}

// ─── Grupo B: SearchCustomers.total_spent — correção do bug ──────────────────

func TestF4_GroupB_SearchCustomers_TotalSpent(t *testing.T) {
	requireCustDB(t)
	ctx := context.Background()
	seed := seedF4Cust(t)

	rows, err := custTestQueries.SearchCustomers(ctx, sqlc.SearchCustomersParams{
		StoreID:        parseCustUUID(t, seed.storeID),
		PlatformHandle: seed.custHandle,
		Limit:          50,
		Offset:         0,
	})
	if err != nil {
		t.Fatalf("SearchCustomers: %v", err)
	}

	if len(rows) == 0 {
		t.Fatal("SearchCustomers: nenhum resultado para handle do customer semeado")
	}

	got := rows[0].TotalSpent

	// Grupo B: NOVO == só-pago
	if got != seed.paidGMV {
		t.Errorf("SearchCustomers total_spent: want %d (paid-only), got %d", seed.paidGMV, got)
	}

	// DIFERE do antigo
	antigoTotal := seed.paidGMV + seed.unpaidGMV
	if got == antigoTotal {
		t.Errorf("SearchCustomers total_spent: got all-carts value (%d) — bug B não corrigido; quer apenas paid (%d)",
			antigoTotal, seed.paidGMV)
	}
}

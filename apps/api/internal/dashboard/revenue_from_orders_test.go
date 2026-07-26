package dashboard_test

// Golden de paridade para Fatia 4 — cutover de leitura Tier-1.
//
// Grupos testados aqui:
//   Grupo A — queries já filtram pago; asserte NOVO(orders) == antigo(cart paid-only)
//   Grupo B — bug latente (somavam não-pagos); asserte NOVO == só-pago E NOVO != antigo
//
// Queries cobertas (dashboard/repository.go inline + SQLC cart/notification):
//   Grupo A: GetEventsWithRevenue, GetAggregatedFunnel, GetEventStats, GetSessionStats,
//            GetWhatsAppRecoveryStats
//   Grupo B: GetStats (total_revenue), GetMonthlyRevenue

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
	"livecart/apps/api/internal/dashboard"
	"livecart/apps/api/lib/database"
)

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

	dbName := fmt.Sprintf("lc_f4dash_test_%d", time.Now().UnixNano())
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

// ─── Seed helpers ─────────────────────────────────────────────────────────────

type f4Seed struct {
	storeID      string
	eventID      string
	sessionID    string
	paidCartID   string
	unpaidCartID string
	orderID      string
	customerID   string
	paidGMV      int64
	unpaidGMV    int64
}

// seedF4 cria um dataset com bordas para os testes de paridade:
//   - 1 store / 1 evento / 1 sessão / 1 customer
//   - Cart pago  (3 × 10000 = 30000) + Order materializada  → Grupo A e B
//   - Cart NÃO-pago (2 × 5000 = 10000, sem Order)           → expõe o bug B
func seedF4(t *testing.T) f4Seed {
	t.Helper()
	ctx := context.Background()
	nRaw := time.Now().UnixNano()
	n := fmt.Sprintf("%d", nRaw)

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('F4 Store', 'f4-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'F4 Event') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	var sessionID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, sequence_order) VALUES ($1, 1) RETURNING id::text`, eventID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	var customerID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO customers (store_id, platform_user_id, platform_handle)
		 VALUES ($1, 'f4cust-'||$2, '@f4cust') RETURNING id::text`, storeID, n,
	).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	// Produto A (para o cart pago) — keyword CHAR(4): 4 dígitos em [1000,9999]
	kwA := fmt.Sprintf("%04d", nRaw%9000+1000)
	var prodAID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'F4ProdA', 'none', 'f4ea-'||$2, $3, 10000, 100) RETURNING id::text`,
		storeID, n, kwA,
	).Scan(&prodAID); err != nil {
		t.Fatalf("seed product A: %v", err)
	}

	// Cart 1: pago, session, customer — 3 itens × 10000 = 30000
	const paidGMV int64 = 30000
	var paidCartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token,
		   short_id, status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents, customer_id)
		 VALUES ($1, $2, 'f4paid-'||$3, '@f4paid', 'f4tok-p-'||$3,
		   ($3::bigint % 90000)+1000, 'checkout', 'paid', now(), 0, 0, $4) RETURNING id::text`,
		eventID, sessionID, n, customerID,
	).Scan(&paidCartID); err != nil {
		t.Fatalf("seed paid cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 3, 10000, 0)`,
		paidCartID, prodAID,
	); err != nil {
		t.Fatalf("seed paid cart items: %v", err)
	}

	// Order 1: selada para o cart pago
	var orderID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, customer_id, status,
		   total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at)
		 VALUES ($1, ($2::bigint % 90000)+10000, $3, $4, $5,
		   'paid', $6, 0, 0, $6, now()) RETURNING id::text`,
		paidCartID, n, storeID, eventID, customerID, paidGMV,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	// Produto B (para o cart NÃO-pago) — keyword CHAR(4): diferente do A
	kwB := fmt.Sprintf("%04d", (nRaw+1)%9000+1000)
	var prodBID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'F4ProdB', 'none', 'f4eb-'||$2, $3, 5000, 100) RETURNING id::text`,
		storeID, n, kwB,
	).Scan(&prodBID); err != nil {
		t.Fatalf("seed product B: %v", err)
	}

	// Cart 2: NÃO-pago, mesmo evento, platform_user_id diferente, mesmo customer
	const unpaidGMV int64 = 10000
	var unpaidCartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token,
		   short_id, status, payment_status, coupon_discount_cents, customer_id)
		 VALUES ($1, 'f4unpd-'||$2, '@f4unpd', 'f4tok-u-'||$2,
		   ($2::bigint % 90000)+50000, 'checkout', 'pending', 0, $3) RETURNING id::text`,
		eventID, n, customerID,
	).Scan(&unpaidCartID); err != nil {
		t.Fatalf("seed unpaid cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 2, 5000, 0)`,
		unpaidCartID, prodBID,
	); err != nil {
		t.Fatalf("seed unpaid cart items: %v", err)
	}

	return f4Seed{
		storeID: storeID, eventID: eventID, sessionID: sessionID,
		paidCartID: paidCartID, unpaidCartID: unpaidCartID,
		orderID: orderID, customerID: customerID,
		paidGMV: paidGMV, unpaidGMV: unpaidGMV,
	}
}

func newRepo(t *testing.T) *dashboard.Repository {
	t.Helper()
	requireDB(t)
	return dashboard.NewRepository(testPool)
}

func parseUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parseUUID(%q): %v", s, err)
	}
	return u
}

// ─── Grupo A: GetEventsWithRevenue ────────────────────────────────────────────

func TestDashboardRevenue_GetEventsWithRevenue(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)
	repo := newRepo(t)

	events, err := repo.GetEventsWithRevenue(ctx, seed.storeID, 10)
	if err != nil {
		t.Fatalf("GetEventsWithRevenue: %v", err)
	}

	var got int64
	for _, e := range events {
		if e.ID == seed.eventID {
			got = e.ConfirmedRevenue
		}
	}

	// Grupo A: NOVO == antigo (cart-based paid-only == orders-based paid)
	if got != seed.paidGMV {
		t.Errorf("GetEventsWithRevenue confirmed_revenue: want %d (paid GMV), got %d", seed.paidGMV, got)
	}

	// Garante que o cart não-pago NÃO contamina o valor
	cartBased := seed.paidGMV + seed.unpaidGMV
	if got == cartBased {
		t.Errorf("GetEventsWithRevenue: got all-carts sum (%d) — bug B não corrigido", cartBased)
	}
}

// ─── Grupo A: GetAggregatedFunnel ─────────────────────────────────────────────

func TestDashboardRevenue_GetAggregatedFunnel(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)
	repo := newRepo(t)

	row, err := repo.GetAggregatedFunnel(ctx, seed.storeID, 365)
	if err != nil {
		t.Fatalf("GetAggregatedFunnel: %v", err)
	}

	// Grupo A: confirmed_revenue NOVO == paid-only truth (antigo era o mesmo valor pois já filtrava)
	if row.ConfirmedRevenue != seed.paidGMV {
		t.Errorf("GetAggregatedFunnel confirmed_revenue: want %d, got %d", seed.paidGMV, row.ConfirmedRevenue)
	}

	// average_ticket == paidGMV / 1 order = paidGMV (há exatamente 1 paid order)
	if row.AverageTicket != seed.paidGMV {
		t.Errorf("GetAggregatedFunnel average_ticket: want %d (1 order), got %d", seed.paidGMV, row.AverageTicket)
	}
}

// ─── Grupo A: GetEventStats.confirmed_revenue ─────────────────────────────────

func TestDashboardRevenue_GetEventStats_ConfirmedRevenue(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)

	row, err := testQueries.GetEventStats(ctx, parseUUID(t, seed.eventID))
	if err != nil {
		t.Fatalf("GetEventStats: %v", err)
	}

	// Grupo A: confirmed_revenue lê de orders.total_cents
	if row.ConfirmedRevenue != seed.paidGMV {
		t.Errorf("GetEventStats confirmed_revenue: want %d, got %d", seed.paidGMV, row.ConfirmedRevenue)
	}

	// Grupo C intacto: projected_revenue é cart-based (carts active/checkout)
	// Cart 2 é unpaid+checkout → deve aparecer na projeção
	if row.ProjectedRevenue == 0 {
		t.Errorf("GetEventStats projected_revenue: expected cart-based projection > 0, got 0")
	}
}

// ─── Grupo A: GetSessionStats.paid_revenue ────────────────────────────────────

func TestDashboardRevenue_GetSessionStats_PaidRevenue(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)

	row, err := testQueries.GetSessionStats(ctx, parseUUID(t, seed.sessionID))
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	// Grupo A: paid_revenue lê de orders.total_cents JOIN carts.session_id
	if row.PaidRevenue != seed.paidGMV {
		t.Errorf("GetSessionStats paid_revenue: want %d, got %d", seed.paidGMV, row.PaidRevenue)
	}

	// Grupo C intacto: total_revenue é cart-based (todos os carts não-expirados da sessão)
	if row.TotalRevenue == 0 {
		t.Errorf("GetSessionStats total_revenue: expected cart-based value > 0, got 0")
	}
}

// ─── Grupo A: GetWhatsAppRecoveryStats.revenue_recovered_cents ───────────────

func TestDashboardRevenue_GetWhatsAppRecoveryStats(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)

	// Seed: notification de recuperação enviada 1h antes do pagamento
	if _, err := testPool.Exec(ctx,
		`INSERT INTO notification_logs
		   (store_id, event_id, cart_id, platform_user_id, platform_handle,
		    notification_type, channel, status, message_text, created_at)
		 VALUES ($1, $2, $3, 'whatsapp-uid', '@f4paid',
		   'cart_recovery', 'whatsapp', 'sent', 'Oi, retome seu carrinho',
		   now() - interval '1 hour')`,
		seed.storeID, seed.eventID, seed.paidCartID,
	); err != nil {
		t.Fatalf("seed notification_log: %v", err)
	}

	row, err := testQueries.GetWhatsAppRecoveryStats(ctx, parseUUID(t, seed.storeID))
	if err != nil {
		t.Fatalf("GetWhatsAppRecoveryStats: %v", err)
	}

	if row.CartsRecovered != 1 {
		t.Errorf("GetWhatsAppRecoveryStats carts_recovered: want 1, got %d", row.CartsRecovered)
	}

	// Grupo A: revenue_recovered_cents lê de orders.total_cents
	if row.RevenueRecoveredCents != seed.paidGMV {
		t.Errorf("GetWhatsAppRecoveryStats revenue_recovered_cents: want %d (order total), got %d",
			seed.paidGMV, row.RevenueRecoveredCents)
	}
}

// ─── Grupo B: GetStats.total_revenue — correção do bug ───────────────────────

func TestDashboardRevenue_GetStats_TotalRevenue(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)
	repo := newRepo(t)

	row, err := repo.GetStats(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	// Grupo B: NOVO == só-pago (correção consciente do bug)
	if row.TotalRevenue != seed.paidGMV {
		t.Errorf("GetStats total_revenue: want %d (paid-only), got %d", seed.paidGMV, row.TotalRevenue)
	}

	// Asserte que DIFERE do antigo (que somava carts não-pagos) — prova que a correção foi aplicada
	antigoTotal := seed.paidGMV + seed.unpaidGMV
	if row.TotalRevenue == antigoTotal {
		t.Errorf("GetStats total_revenue: got all-carts value (%d) — bug B não corrigido; quer apenas paid (%d)",
			antigoTotal, seed.paidGMV)
	}
}

// ─── Grupo B: GetMonthlyRevenue — correção do bug ────────────────────────────

func TestDashboardRevenue_GetMonthlyRevenue(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF4(t)
	repo := newRepo(t)

	rows, err := repo.GetMonthlyRevenue(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetMonthlyRevenue: %v", err)
	}

	if len(rows) == 0 {
		t.Fatal("GetMonthlyRevenue: esperava ao menos 1 linha (mês atual), mas não veio nenhuma")
	}

	// Soma total: apenas pedidos pagos no ano
	var novoTotal int64
	for _, r := range rows {
		novoTotal += r.Revenue
	}

	// Grupo B: NOVO == só-pago
	if novoTotal != seed.paidGMV {
		t.Errorf("GetMonthlyRevenue soma: want %d (paid-only), got %d", seed.paidGMV, novoTotal)
	}

	// Asserte que DIFERE do antigo (que somava todos os carts)
	antigoTotal := seed.paidGMV + seed.unpaidGMV
	if novoTotal == antigoTotal {
		t.Errorf("GetMonthlyRevenue soma: got all-carts value (%d) — bug B não corrigido; quer apenas paid (%d)",
			antigoTotal, seed.paidGMV)
	}
}

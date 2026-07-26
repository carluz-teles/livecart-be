package postcheckout_test

// Fatia 10-a — carts.tracking_token dropado; a fonte da verdade do rastreio é
// order_logistics.tracking_token (Fatia C1). Gated em TEST_DATABASE_URL (mesma
// infra dos testes fatia_b1/on_cart_paid do pacote order).
//
// Invariantes travadas:
//   C1a FONTE ÚNICA: OnCartPaid grava o token SÓ em order_logistics (não-nulo),
//       e o payment_confirmed carrega order_id == orders.id.
//   C1b IDEMPOTÊNCIA: dupla/tripla execução de OnCartPaid não duplica eventos
//       nem gera dois tokens (shortcut via order_logistics.tracking_token).
//   C1c RASTREIO (AC1/AC2): o lookup público acha o cart pelo token de
//       order_logistics e devolve o token resolvido; token errado → não acha.
//   C1d UNIQUE POR ORDER: InsertOrderEvent dedupa por (order_id, event_type).
//   C1e DEFERRAL: OnCartPaid num cart pago SEM Order materializada não grava
//       token nem evento (adia p/ o reactor async) — replay-safe.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/order/listeners"
	"livecart/apps/api/internal/postcheckout"
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/email"
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

	dbName := fmt.Sprintf("lc_postcheckout_test_%d", time.Now().UnixNano())
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

// newService monta o Service de post-checkout com um cliente de e-mail real (não
// faz rede: os carts semeados não têm customer_email, então o envio é pulado).
func newService(t *testing.T) *postcheckout.Service {
	t.Helper()
	return postcheckout.NewService(postcheckout.NewRepository(testQueries), email.NewClient(zap.NewNop()), zap.NewNop())
}

// seedPaidCartWithOrder cria loja→evento→produto→cart pago (sem customer_email,
// para pular o envio de e-mail) e materializa a Order via o listener real — o
// mesmo caminho do webhook, onde a Order já existe quando o post-checkout roda.
// Retorna o cartID.
func seedPaidCartWithOrder(t *testing.T) (cartID string) {
	t.Helper()
	ctx := context.Background()
	cartID, storeID := seedPaidCart(t)

	// Materializa a Order (Fase A) — order + order_logistics passam a existir,
	// tracking_token nasce NULL (Fatia 10-a: order_logistics é a fonte, o cart
	// nunca carregou token; o postcheckout o gera depois).
	l := listeners.New(testPool, testQueries, zap.NewNop())
	if err := l.OnCartPaid(ctx, cartID, storeID, 10000, nil); err != nil {
		t.Fatalf("materialise order: %v", err)
	}
	return cartID
}

// seedPaidCart cria loja→evento→produto→cart pago SEM materializar a Order —
// espelha o path síncrono do cartão, onde o post-checkout roda antes do fato
// cart.paid criar a Order. Retorna (cartID, storeID).
func seedPaidCart(t *testing.T) (cartID, storeID string) {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Loja PC Test', 'pc-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'Ev') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	kw := fmt.Sprintf("%d", time.Now().UnixNano()%8000+1000)
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Prod', 'none', 'ext-'||$2, $3, 5000, 100) RETURNING id::text`,
		storeID, n, kw,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, paid_at)
		 VALUES ($1, 'u-'||$2, '@b'||$2, 'tok-'||$2, ($2)::bigint % 100000,
		   'checkout', 'paid', now()) RETURNING id::text`,
		eventID, n,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 2, 5000, 0)`,
		cartID, productID,
	); err != nil {
		t.Fatalf("seed cart_items: %v", err)
	}

	return cartID, storeID
}

// logisticsToken lê o token da fonte da verdade (order_logistics) para um cart.
func logisticsToken(t *testing.T, ctx context.Context, cartID string) (string, bool) {
	t.Helper()
	var tok *string
	if err := testPool.QueryRow(ctx, `
		SELECT ol.tracking_token
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.cart_id = $1`, cartID,
	).Scan(&tok); err != nil {
		t.Fatalf("query logistics token: %v", err)
	}
	if tok == nil {
		return "", false
	}
	return *tok, true
}

// ─── C1a FONTE ÚNICA (order_logistics) ────────────────────────────────────────

func TestOnCartPaid_C1a_WritesTokenOnlyOnOrderLogistics(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	newService(t).OnCartPaid(ctx, cartID)

	// payment_confirmed existe, único, e carrega order_id == orders.id.
	var eventCount int
	var eventOrderID, ordersID *string
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*),
		       MAX(oe.order_id::text),
		       MAX(o.id::text)
		FROM order_events oe
		JOIN orders o ON o.cart_id = oe.cart_id
		WHERE oe.cart_id = $1 AND oe.event_type = 'payment_confirmed'`, cartID,
	).Scan(&eventCount, &eventOrderID, &ordersID); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("C1a: expected 1 payment_confirmed event, got %d", eventCount)
	}
	if eventOrderID == nil {
		t.Fatalf("C1a: payment_confirmed.order_id is NULL, want == orders.id")
	}
	if ordersID == nil || *eventOrderID != *ordersID {
		t.Errorf("C1a: order_id mismatch: event=%v orders=%v", eventOrderID, ordersID)
	}

	// Token gravado em order_logistics, não-nulo.
	tok, ok := logisticsToken(t, ctx, cartID)
	if !ok || tok == "" {
		t.Fatalf("C1a: order_logistics.tracking_token vazio")
	}
}

// ─── C1b IDEMPOTÊNCIA ─────────────────────────────────────────────────────────

func TestOnCartPaid_C1b_IdempotentNoDuplicateEventsOrTokens(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	svc := newService(t)
	svc.OnCartPaid(ctx, cartID)
	first, _ := logisticsToken(t, ctx, cartID)

	// Re-executa: replay de webhook / mark-as-paid manual.
	for i := 0; i < 2; i++ {
		svc.OnCartPaid(ctx, cartID)
	}

	var eventCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_events WHERE cart_id = $1 AND event_type = 'payment_confirmed'`, cartID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("C1b: expected 1 payment_confirmed event após 3× OnCartPaid, got %d", eventCount)
	}

	// Token estável (não rotacionou) e único em order_logistics.
	after, _ := logisticsToken(t, ctx, cartID)
	if first == "" || after != first {
		t.Errorf("C1b: token rotacionou no replay: first=%q after=%q", first, after)
	}
}

// ─── C1c RASTREIO via order_logistics (AC1/AC2) ───────────────────────────────

func TestGetCartByTrackingToken_C1c_ResolvesViaOrderLogistics(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	repo := postcheckout.NewRepository(testQueries)
	newService(t).OnCartPaid(ctx, cartID)

	token, ok := logisticsToken(t, ctx, cartID)
	if !ok || token == "" {
		t.Fatalf("C1c: token de order_logistics vazio")
	}

	// Token certo → acha o cart e devolve o token resolvido (que o handler compara
	// constant-time contra o input).
	cart, resolved, err := repo.GetCartByTrackingToken(ctx, token)
	if err != nil {
		t.Fatalf("GetCartByTrackingToken: %v", err)
	}
	if cart == nil {
		t.Fatalf("C1c (AC1): cart não encontrado via order_logistics")
	}
	if cart.ID.String() != cartID {
		t.Errorf("C1c (AC1): cart errado: got %s want %s", cart.ID.String(), cartID)
	}
	if resolved != token {
		t.Errorf("C1c (AC2): token resolvido=%q, want %q", resolved, token)
	}

	// Token errado → não acha (404 sem vazar existência).
	cart, resolved, err = repo.GetCartByTrackingToken(ctx, token+"-wrong")
	if err != nil {
		t.Fatalf("GetCartByTrackingToken (wrong): %v", err)
	}
	if cart != nil || resolved != "" {
		t.Errorf("C1c (AC1): token errado deveria não achar; got cart=%v resolved=%q", cart, resolved)
	}
}

// ─── C1d UNIQUE por (order_id, event_type) ────────────────────────────────────

func TestInsertOrderEvent_C1d_UniqueByOrderID(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	repo := postcheckout.NewRepository(testQueries)
	orderID, err := repo.ResolveOrderID(ctx, cartID)
	if err != nil {
		t.Fatalf("ResolveOrderID: %v", err)
	}
	if !orderID.Valid {
		t.Fatalf("C1d: ResolveOrderID retornou order_id inválido para Order materializada")
	}

	first, err := repo.InsertOrderEvent(ctx, orderID, cartID, "shipped", "merchant", nil)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !first {
		t.Errorf("C1d: primeiro insert deveria retornar inserted=true")
	}

	second, err := repo.InsertOrderEvent(ctx, orderID, cartID, "shipped", "merchant", nil)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if second {
		t.Errorf("C1d: segundo insert do mesmo (order_id, event_type) deveria dedupar (inserted=false)")
	}

	var count int
	testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_events WHERE cart_id = $1 AND event_type = 'shipped'`, cartID,
	).Scan(&count)
	if count != 1 {
		t.Errorf("C1d: expected 1 shipped event, got %d", count)
	}
}

// ─── C1e DEFERRAL: sem Order materializada ────────────────────────────────────

// OnCartPaid no path síncrono do cartão (Order ainda não materializada) não pode
// persistir o token de forma rastreável, então adia TODO o fluxo — não grava
// token nem evento. O reactor async (que materializa a Order antes) faz o resto.
func TestOnCartPaid_C1e_DefersWhenOrderNotMaterialised(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID, _ := seedPaidCart(t) // sem materializar a Order

	newService(t).OnCartPaid(ctx, cartID)

	// Nenhuma Order/order_logistics ainda → nada de token.
	if _, ok := logisticsTokenIfAny(t, ctx, cartID); ok {
		t.Errorf("C1e: order_logistics não deveria existir/ter token sem materialização")
	}

	// Nenhum payment_confirmed gravado (fluxo adiado, replay-safe).
	var eventCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_events WHERE cart_id = $1 AND event_type = 'payment_confirmed'`, cartID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("C1e: expected 0 payment_confirmed events (deferred), got %d", eventCount)
	}
}

// logisticsTokenIfAny é como logisticsToken, mas não falha quando não há
// order_logistics (ok=false) — usado no cenário de deferral.
func logisticsTokenIfAny(t *testing.T, ctx context.Context, cartID string) (string, bool) {
	t.Helper()
	var tok *string
	err := testPool.QueryRow(ctx, `
		SELECT ol.tracking_token
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.cart_id = $1`, cartID,
	).Scan(&tok)
	if err != nil {
		return "", false // sem Order/order_logistics
	}
	if tok == nil {
		return "", false
	}
	return *tok, true
}

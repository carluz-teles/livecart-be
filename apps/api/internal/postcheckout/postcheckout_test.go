package postcheckout_test

// Fatia C1 — order_events keyed pela Order (order_id) + tracking_token com fonte
// da verdade em order_logistics. Gated em TEST_DATABASE_URL (mesma infra dos
// testes fatia_b1/on_cart_paid do pacote order).
//
// Invariantes travadas:
//   C1a DUAL-WRITE: OnCartPaid grava o token no cart E em order_logistics
//       (iguais), e o payment_confirmed carrega order_id == orders.id.
//   C1b IDEMPOTÊNCIA (AC5): dupla/tripla execução de OnCartPaid não duplica
//       eventos nem gera dois tokens (o shortcut do tracking_token continua).
//   C1c RASTREIO (AC4): o lookup público acha o cart pelo token de
//       order_logistics mesmo quando carts.tracking_token já foi limpo.
//   C1d UNIQUE POR ORDER: InsertOrderEvent dedupa por (order_id, event_type).

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
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID string
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

	// Materializa a Order (Fase A) — order + order_logistics passam a existir,
	// tracking_token ainda NULL (o cart não tem token nesse momento).
	l := listeners.New(testPool, testQueries, zap.NewNop())
	if err := l.OnCartPaid(ctx, cartID, storeID, 10000, nil); err != nil {
		t.Fatalf("materialise order: %v", err)
	}
	return cartID
}

// ─── C1a DUAL-WRITE ───────────────────────────────────────────────────────────

func TestOnCartPaid_C1a_DualWritesOrderIDAndTrackingToken(t *testing.T) {
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

	// Token gravado no cart E em order_logistics, iguais e não-nulos.
	var cartToken, logisticsToken *string
	if err := testPool.QueryRow(ctx, `
		SELECT c.tracking_token, ol.tracking_token
		FROM carts c
		JOIN orders o ON o.cart_id = c.id
		JOIN order_logistics ol ON ol.order_id = o.id
		WHERE c.id = $1`, cartID,
	).Scan(&cartToken, &logisticsToken); err != nil {
		t.Fatalf("query tokens: %v", err)
	}
	if cartToken == nil || *cartToken == "" {
		t.Fatalf("C1a: carts.tracking_token vazio")
	}
	if logisticsToken == nil || *logisticsToken != *cartToken {
		t.Errorf("C1a: order_logistics.tracking_token=%v, want == carts.tracking_token=%v", logisticsToken, cartToken)
	}
}

// ─── C1b IDEMPOTÊNCIA (AC5) ───────────────────────────────────────────────────

func TestOnCartPaid_C1b_IdempotentNoDuplicateEventsOrTokens(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	svc := newService(t)
	for i := 0; i < 3; i++ {
		svc.OnCartPaid(ctx, cartID)
	}

	var eventCount, tokenCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_events WHERE cart_id = $1 AND event_type = 'payment_confirmed'`, cartID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("C1b (AC5): expected 1 payment_confirmed event após 3× OnCartPaid, got %d", eventCount)
	}

	// Um único token, estável, refletido em order_logistics.
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM carts c
		JOIN orders o ON o.cart_id = c.id
		JOIN order_logistics ol ON ol.order_id = o.id
		WHERE c.id = $1
		  AND c.tracking_token IS NOT NULL
		  AND ol.tracking_token = c.tracking_token`, cartID,
	).Scan(&tokenCount); err != nil {
		t.Fatalf("query token: %v", err)
	}
	if tokenCount != 1 {
		t.Errorf("C1b (AC5): expected 1 cart com token consistente em order_logistics, got %d", tokenCount)
	}
}

// ─── C1c RASTREIO via order_logistics (AC4) ───────────────────────────────────

func TestGetCartByTrackingToken_C1c_ResolvesViaOrderLogistics(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	repo := postcheckout.NewRepository(testQueries)
	newService(t).OnCartPaid(ctx, cartID)

	// Lê o token da FONTE DA VERDADE (order_logistics).
	var token string
	if err := testPool.QueryRow(ctx, `
		SELECT ol.tracking_token
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.cart_id = $1`, cartID,
	).Scan(&token); err != nil {
		t.Fatalf("query logistics token: %v", err)
	}

	// Simula a Fase F: limpa carts.tracking_token para provar que o lookup passa
	// por order_logistics, não pelo cart.
	if _, err := testPool.Exec(ctx, `UPDATE carts SET tracking_token = NULL WHERE id = $1`, cartID); err != nil {
		t.Fatalf("blank cart token: %v", err)
	}

	cart, err := repo.GetCartByTrackingToken(ctx, token)
	if err != nil {
		t.Fatalf("GetCartByTrackingToken: %v", err)
	}
	if cart == nil {
		t.Fatalf("C1c (AC4): cart não encontrado via order_logistics após limpar carts.tracking_token")
	}
	if cart.ID.String() != cartID {
		t.Errorf("C1c (AC4): cart errado: got %s want %s", cart.ID.String(), cartID)
	}
	// O comparativo constant-time do handler bate contra cart.TrackingToken.
	if cart.TrackingToken.String != token {
		t.Errorf("C1c (AC4): TrackingToken retornado=%q, want %q", cart.TrackingToken.String, token)
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

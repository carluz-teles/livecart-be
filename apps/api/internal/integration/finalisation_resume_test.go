package integration

// Testes de integração da máquina de estados da finalização ERP (Fase 2 do
// fix do race de reservas — dossiê 11/07/2026).
//
// Rodam contra um Postgres REAL com as migrations completas aplicadas num
// database descartável, e um ERP roteirizado (scriptedERP) no lugar do Tiny.
// Cada cenário derruba a finalização num passo específico e prova que o retry
// converge para 'done' sem duplicar pedido, baixa ou entrada de estoque.
//
// Pré-requisito local:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' go test ./apps/api/internal/integration/ -run Finalisation -v
//
// Sem TEST_DATABASE_URL os testes fazem Skip (CI sem banco continua verde).

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product"
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/httpx"
)

// ============================================================================
// Infra: um database descartável por execução do binário de teste
// ============================================================================

var (
	testPool *pgxpool.Pool
	testRepo *Repository
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		// Testes individuais dão t.Skip quando testPool == nil.
		return m.Run()
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TEST_DATABASE_URL inválida: %v\n", err)
		return 1
	}
	defer admin.Close()

	dbName := fmt.Sprintf("lc_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		fmt.Fprintf(os.Stderr, "criando database de teste: %v\n", err)
		return 1
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	}()

	u, err := url.Parse(adminURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parseando TEST_DATABASE_URL: %v\n", err)
		return 1
	}
	u.Path = "/" + dbName
	testURL := u.String()

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "db", "migrations")
	if err := database.RunMigrations(testURL, migrationsPath); err != nil {
		fmt.Fprintf(os.Stderr, "migrations no database de teste: %v\n", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conectando no database de teste: %v\n", err)
		return 1
	}
	defer pool.Close()

	testPool = pool
	testRepo = NewRepository(sqlc.New(pool), pool)
	return m.Run()
}

func requireDB(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não setada — suba `docker compose up -d postgres` e exporte a URL")
	}
}

// ============================================================================
// Seeds
// ============================================================================

type finFixture struct {
	storeID   string
	eventID   string
	productID string
	cartID    string
}

var seedSeq int

func seedPaidCart(t *testing.T, qty, activeReservations int) finFixture {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000, seedSeq)

	var fx finFixture
	mustScan := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("seed %q: %v", sql[:min(60, len(sql))], err)
		}
	}

	mustScan(&fx.storeID,
		`INSERT INTO stores (name, slug) VALUES ('Loja Teste', 'loja-'||$1) RETURNING id::text`, n)
	mustScan(new(string),
		`INSERT INTO integrations (store_id, type, provider, status) VALUES ($1, 'erp', 'tiny', 'active') RETURNING id::text`, fx.storeID)
	mustScan(&fx.eventID,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'Live Teste') RETURNING id::text`, fx.storeID)
	kw := fmt.Sprintf("%d", 1000+seedSeq%9000) // keyword numérica 1000-9999 (regra do domínio)
	mustScan(&fx.productID,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Produto Teste', 'tiny', 'EXT-'||$2, $3, 1000, 10) RETURNING id::text`, fx.storeID, n, kw)
	mustScan(&fx.cartID,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status, paid_at)
		 VALUES ($1, 'user-'||$2, '@buyer'||$2, 'tok-'||$2, ($2)::bigint % 100000, 'checkout', 'paid', now()) RETURNING id::text`,
		fx.eventID, n)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, 1000, 0)`, fx.cartID, fx.productID, qty); err != nil {
		t.Fatalf("seed cart_items: %v", err)
	}
	for i := 0; i < activeReservations; i++ {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, erp_movement_id, status)
			 VALUES ($1, $2, $3, 'EXT-'||$4, $5, 'MOV-SEED-'||$4, 'active')`,
			fx.eventID, fx.cartID, fx.productID, n, qty); err != nil {
			t.Fatalf("seed stock_reservations: %v", err)
		}
	}
	return fx
}

func cartFinalisationState(t *testing.T, cartID string) (status, lastError, externalOrderID string, attempts int, hasSnapshot bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT erp_finalisation_status, COALESCE(erp_last_error,''), COALESCE(external_order_id,''),
		        erp_attempts_count, erp_payment_snapshot IS NOT NULL
		 FROM carts WHERE id = $1`, cartID).
		Scan(&status, &lastError, &externalOrderID, &attempts, &hasSnapshot)
	if err != nil {
		t.Fatalf("lendo estado do cart: %v", err)
	}
	return
}

func activeReservationCount(t *testing.T, cartID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM stock_reservations WHERE cart_id = $1 AND status = 'active'`, cartID).Scan(&n); err != nil {
		t.Fatalf("contando reservas: %v", err)
	}
	return n
}

// ============================================================================
// ERP roteirizado
// ============================================================================

type scriptedERP struct {
	providers.ERPProvider // nil: método não roteirizado = panic (chamada inesperada)

	mu           sync.Mutex
	calls        []string
	failures     map[string]int // método -> quantas próximas chamadas falham
	createDelay  time.Duration
	orderSeq     int
	markerOrders map[string]string // marcador -> orderID (FindOrderIDByMarker)
}

func newScriptedERP() *scriptedERP {
	return &scriptedERP{failures: map[string]int{}}
}

func (f *scriptedERP) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *scriptedERP) scriptedFail(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures[name] > 0 {
		f.failures[name]--
		return fmt.Errorf("scripted failure: %s", name)
	}
	return nil
}

func (f *scriptedERP) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// firstIndex devolve a posição da primeira chamada com o prefixo (-1 = nunca).
func (f *scriptedERP) firstIndex(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func (f *scriptedERP) CreateOrder(ctx context.Context, order providers.ERPOrder) (*providers.OrderResult, error) {
	f.record("CreateOrder")
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	if err := f.scriptedFail("CreateOrder"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.orderSeq++
	id := fmt.Sprintf("ORD-%d", f.orderSeq)
	f.mu.Unlock()
	return &providers.OrderResult{OrderID: id, OrderNumber: id, Status: "created"}, nil
}

func (f *scriptedERP) LaunchOrderStock(ctx context.Context, orderID string) error {
	f.record("Launch:" + orderID)
	if err := f.scriptedFail("LaunchOrderStock"); err != nil {
		return err
	}
	// O cliente real trata "Estoque já lançado." como sucesso (validado em
	// sandbox 11/07) — o fake devolve nil na repetição, espelhando isso.
	return nil
}

func (f *scriptedERP) ReverseStockReservation(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error) {
	f.record("ReverseRes:" + productID)
	if err := f.scriptedFail("ReverseStockReservation"); err != nil {
		return "", err
	}
	return "MOV-R", nil
}

func (f *scriptedERP) ReserveStock(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error) {
	f.record("ReReserve:" + productID)
	if err := f.scriptedFail("ReserveStock"); err != nil {
		return "", err
	}
	return "MOV-S", nil
}

func (f *scriptedERP) ApproveOrder(ctx context.Context, orderID string) error { return nil }

func (f *scriptedERP) SearchContacts(ctx context.Context, params providers.SearchContactsParams) ([]providers.ERPContactResult, error) {
	f.record("SearchContacts")
	return nil, nil
}

func (f *scriptedERP) CreateContact(ctx context.Context, contact providers.ERPContactInput) (*providers.ERPContactResult, error) {
	f.record("CreateContact")
	return &providers.ERPContactResult{ContactID: "9001", Name: contact.Name}, nil
}

func (f *scriptedERP) UpdateContact(ctx context.Context, contactID string, contact providers.ERPContactInput) error {
	f.record("UpdateContact")
	return nil
}

func newFinalisationService(fake providers.ERPProvider) *Service {
	return &Service{
		repo:   testRepo,
		logger: zap.NewNop(),
		stock:  NewStockReservations(testRepo, zap.NewNop()),
		erpProviderFactory: func(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
			return fake, nil
		},
	}
}

func testPaymentStatus() *providers.PaymentStatus {
	paidAt := time.Now()
	return &providers.PaymentStatus{
		PaymentID:     "PAY-1",
		Amount:        2000,
		PaidAt:        &paidAt,
		PaymentMethod: "pix",
	}
}

// ============================================================================
// Cenários
// ============================================================================

func TestFinalisationHappyPath(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	status, _, orderID, attempts, hasSnapshot := cartFinalisationState(t, fx.cartID)
	if status != "done" {
		t.Fatalf("status = %q, esperado done", status)
	}
	if orderID != "ORD-1" {
		t.Fatalf("external_order_id = %q, esperado ORD-1", orderID)
	}
	if attempts != 1 || !hasSnapshot {
		t.Fatalf("attempts=%d hasSnapshot=%v — S1 não persistiu", attempts, hasSnapshot)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active = %d, esperado 0", n)
	}
	if fake.count("ReverseRes") != 1 || fake.count("CreateOrder") != 1 || fake.count("Launch") != 1 {
		t.Fatalf("chamadas inesperadas: %v", fake.calls)
	}
	// Ordem LEGADA: estorna antes de criar (o pico de saldo existe, guard segura).
	if fake.firstIndex("ReverseRes") > fake.firstIndex("CreateOrder") {
		t.Fatalf("ordem legada violada: %v", fake.calls)
	}
}

func TestFinalisationReversalFailureIsResumable(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["ReverseStockReservation"] = 1
	svc := newFinalisationService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("esperava erro na primeira tentativa (estorno falho)")
	}
	status, lastErr, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "failed" || !strings.Contains(lastErr, "estorno") {
		t.Fatalf("status=%q lastErr=%q — esperado failed retomável de estorno", status, lastErr)
	}
	if orderID != "" || fake.count("CreateOrder") != 0 {
		t.Fatalf("pedido não deveria ter sido criado com estorno pendente (orderID=%q, creates=%d)", orderID, fake.count("CreateOrder"))
	}
	if n := activeReservationCount(t, fx.cartID); n != 1 {
		t.Fatalf("reserva deveria continuar active (n=%d)", n)
	}

	// Retry admin: converge para done, criando o pedido exatamente uma vez.
	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, _, orderID, _, _ = cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-1" {
		t.Fatalf("pós-retry: status=%q orderID=%q", status, orderID)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active pós-retry = %d", n)
	}
	if fake.count("CreateOrder") != 1 || fake.count("Launch") != 1 {
		t.Fatalf("create/launch duplicados: %v", fake.calls)
	}
}

func TestFinalisationCreateFailureRetries(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["CreateOrder"] = 1
	svc := newFinalisationService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("esperava erro na primeira tentativa (create falho)")
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "failed" || orderID != "" {
		t.Fatalf("status=%q orderID=%q", status, orderID)
	}
	// Estoque de cart pago nunca é solto: a re-reserva recriou a saída manual
	// e uma row active nova.
	if fake.count("ReReserve") != 1 {
		t.Fatalf("re-reserva não rodou: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 1 {
		t.Fatalf("reserva re-criada esperada (n=%d)", n)
	}

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, _, orderID, _, _ = cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID == "" {
		t.Fatalf("pós-retry: status=%q orderID=%q", status, orderID)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active pós-retry = %d", n)
	}
}

// Gap B: LaunchOrderStock falha DEPOIS de external_order_id gravado. Antes da
// Fase 2 o retry batia na idempotência ("cart already has ERP order") e
// retornava nil sem terminar nada. Agora: RESUME re-lança sem duplicar pedido.
func TestFinalisationLaunchFailureResumesWithoutDuplicateOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["LaunchOrderStock"] = 1
	svc := newFinalisationService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("esperava erro na primeira tentativa (launch falho)")
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "failed" || orderID != "ORD-1" {
		t.Fatalf("status=%q orderID=%q — esperado failed com pedido gravado", status, orderID)
	}

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, _, orderID, _, _ = cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-1" {
		t.Fatalf("pós-retry: status=%q orderID=%q", status, orderID)
	}
	if fake.count("CreateOrder") != 1 {
		t.Fatalf("RESUME duplicou o pedido: %v", fake.calls)
	}
	if fake.count("Launch") != 2 {
		t.Fatalf("esperava 2 chamadas de launch (falha + resume): %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("re-reserva não foi estornada no resume (n=%d)", n)
	}
}

// Gap A: processo morreu entre gravar external_order_id e lançar o estoque —
// cart zumbi 'pending' com pedido no Tiny. Antes da Fase 2 o retry recusava
// ('aguarde a finalização inicial'). Agora o gate reconhece e o resume fecha,
// mesmo SEM snapshot de pagamento (carts pré-deploy).
func TestFinalisationZombiePendingResumes(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET external_order_id = 'ORD-99' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed zumbi: %v", err)
	}
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry do zumbi: %v", err)
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-99" {
		t.Fatalf("status=%q orderID=%q", status, orderID)
	}
	if fake.count("CreateOrder") != 0 {
		t.Fatalf("resume não pode criar pedido novo: %v", fake.calls)
	}
	if fake.count("Launch:ORD-99") != 1 {
		t.Fatalf("launch tolerante do pedido existente não rodou: %v", fake.calls)
	}
}

// Webhooks de gateway chegam duplicados e cada entrega roda numa goroutine
// própria sem lock (webhook_handler.go). O advisory lock por cart garante
// exatamente UMA finalização; a perdedora retorna nil cedo.
func TestFinalisationConcurrentWebhooksSingleOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.createDelay = 300 * time.Millisecond
	svc := newFinalisationService(fake)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	status, _, _, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" {
		t.Fatalf("status = %q", status)
	}
	if fake.count("CreateOrder") != 1 || fake.count("Launch") != 1 || fake.count("ReverseRes") != 1 {
		t.Fatalf("finalização dupla vazou: %v", fake.calls)
	}
}

// Pending FRESCO (finalização inicial rodando agora) continua bloqueado no
// retry — a Fase 2 só liberou zumbis (pedido existente ou tentativa velha).
func TestFinalisationRetryGateFreshPending(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET erp_last_attempt_at = now() WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	svc := newFinalisationService(newScriptedERP())

	err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID)
	var svcErr *httpx.ServiceError
	if err == nil || !errorsAs(err, &svcErr) || !strings.Contains(err.Error(), "aguarde") {
		t.Fatalf("esperava 422 'aguarde', veio: %v", err)
	}
}

// O guard da Fase 1 arma durante a finalização em voo e desarma no done — a
// promoção fantasma da timeline t0-t8 depende exatamente desses dois flips.
func TestStockGuardArmsDuringFinalisationWindow(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	ctx := context.Background()

	extID := extProductID(t, fx.productID)

	guarded, err := testRepo.HasStockGuardForProduct(ctx, extID, fx.storeID, "tiny")
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if !guarded {
		t.Fatal("guard deveria estar ARMADO com cart paid+pending contendo o produto")
	}
	inFlight, err := testRepo.HasInFlightFinalisationForProduct(ctx, fx.productID)
	if err != nil || !inFlight {
		t.Fatalf("in-flight esperado true (err=%v)", err)
	}

	if err := testRepo.MarkCartERPFinalisationDone(ctx, fx.cartID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	guarded, err = testRepo.HasStockGuardForProduct(ctx, extID, fx.storeID, "tiny")
	if err != nil {
		t.Fatalf("guard pós-done: %v", err)
	}
	if guarded {
		t.Fatal("guard deveria DESARMAR após done")
	}
}

func extProductID(t *testing.T, productID string) string {
	t.Helper()
	var ext string
	if err := testPool.QueryRow(context.Background(),
		`SELECT external_id FROM products WHERE id = $1`, productID).Scan(&ext); err != nil {
		t.Fatalf("lendo external_id: %v", err)
	}
	return ext
}

// errorsAs é um wrapper mínimo para não importar errors só por isso.
func errorsAs(err error, target any) bool {
	type causer interface{ Unwrap() error }
	for err != nil {
		if se, ok := err.(*httpx.ServiceError); ok {
			if t, ok := target.(**httpx.ServiceError); ok {
				*t = se
				return true
			}
		}
		if c, ok := err.(causer); ok {
			err = c.Unwrap()
		} else {
			return false
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ============================================================================
// Fase 3 — finalização invertida (launch-first, flag por loja)
// ============================================================================

func newInvertedService(fake providers.ERPProvider) *Service {
	svc := newFinalisationService(fake)
	svc.invertFinalisationAll = true
	return svc
}

// A inversão elimina o pico de saldo: o pedido é criado e LANÇADO antes de
// qualquer entrada E de estorno.
func TestInvertedHappyPathLaunchBeforeReverse(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newInvertedService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("finalize invertida: %v", err)
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-1" {
		t.Fatalf("status=%q orderID=%q", status, orderID)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active = %d", n)
	}
	iCreate, iLaunch, iReverse := fake.firstIndex("CreateOrder"), fake.firstIndex("Launch"), fake.firstIndex("ReverseRes")
	if !(iCreate < iLaunch && iLaunch < iReverse) {
		t.Fatalf("ordem invertida violada (create=%d launch=%d reverse=%d): %v", iCreate, iLaunch, iReverse, fake.calls)
	}
	if fake.count("ReReserve") != 0 {
		t.Fatalf("inverted não compensa nada: %v", fake.calls)
	}
}

// Falha de CreateOrder no fluxo invertido: as reservas nunca foram tocadas —
// zero compensação, zero movimentação; retry converge.
func TestInvertedCreateFailureNoCompensation(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["CreateOrder"] = 1
	svc := newInvertedService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("esperava erro (create falho)")
	}
	status, _, _, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "failed" {
		t.Fatalf("status = %q", status)
	}
	if fake.count("ReverseRes") != 0 || fake.count("ReReserve") != 0 || fake.count("Launch") != 0 {
		t.Fatalf("fluxo invertido tocou estoque em falha de create: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 1 {
		t.Fatalf("reserva deveria seguir intacta (n=%d)", n)
	}

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID == "" {
		t.Fatalf("pós-retry: status=%q orderID=%q", status, orderID)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas pós-retry = %d", n)
	}
}

// Launch falha (conta que bloqueia negativo, saldo preso nas reservas): o
// fallback estorna PRIMEIRO e re-lança — degrada para a ordem legada e a
// finalização ainda conclui na MESMA chamada.
func TestInvertedLaunchFallbackReversesFirst(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["LaunchOrderStock"] = 1
	svc := newInvertedService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("fallback deveria concluir sem erro: %v", err)
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-1" {
		t.Fatalf("status=%q orderID=%q", status, orderID)
	}
	if fake.count("Launch") != 2 {
		t.Fatalf("esperava launch falho + re-launch: %v", fake.calls)
	}
	if fake.firstIndex("ReverseRes") < fake.firstIndex("Launch") {
		t.Fatalf("estorno deveria vir só APÓS o primeiro launch falhar: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active = %d", n)
	}
}

// Launch falha 2× (fallback esgotado): failed com pedido gravado; o retry
// entra pelo RESUME e termina sem duplicar o pedido.
func TestInvertedLaunchFallbackExhaustedThenResume(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["LaunchOrderStock"] = 2
	svc := newInvertedService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("esperava erro (fallback esgotado)")
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "failed" || orderID != "ORD-1" {
		t.Fatalf("status=%q orderID=%q", status, orderID)
	}
	// As reservas foram estornadas durante o fallback — não devem sobrar rows.
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active pós-fallback = %d", n)
	}

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, _, orderID, _, _ = cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-1" {
		t.Fatalf("pós-retry: status=%q orderID=%q", status, orderID)
	}
	if fake.count("CreateOrder") != 1 {
		t.Fatalf("resume duplicou pedido: %v", fake.calls)
	}
	if fake.count("Launch") != 3 {
		t.Fatalf("esperava 3 launches (2 falhos + resume): %v", fake.calls)
	}
}

// Estorno falha DEPOIS do launch: failed retomável; resume re-lança
// (tolerado) e completa os estornos.
func TestInvertedReversalFailureAfterLaunchResumable(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["ReverseStockReservation"] = 1
	svc := newInvertedService(fake)

	if err := svc.finalizeCartERPOrder(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("esperava erro (estorno falho pós-launch)")
	}
	status, lastErr, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "failed" || orderID != "ORD-1" || !strings.Contains(lastErr, "estorno") {
		t.Fatalf("status=%q orderID=%q lastErr=%q", status, orderID, lastErr)
	}
	if n := activeReservationCount(t, fx.cartID); n != 1 {
		t.Fatalf("reserva deveria seguir active (n=%d)", n)
	}

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, _, _, _, _ = cartFinalisationState(t, fx.cartID)
	if status != "done" {
		t.Fatalf("pós-retry: status=%q", status)
	}
	if fake.count("CreateOrder") != 1 || fake.count("Launch") != 2 {
		t.Fatalf("resume errado: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas pós-resume = %d", n)
	}
}

// ============================================================================
// Design C — pedido-como-reserva (conversão na iniciação do pagamento)
// ============================================================================

func (f *scriptedERP) UpdateOrderItems(ctx context.Context, orderID string, items []providers.ERPOrderItem) error {
	f.record("PutItens:" + orderID)
	if err := f.scriptedFail("UpdateOrderItems"); err != nil {
		return err
	}
	return nil
}

func (f *scriptedERP) UpdateOrderPayment(ctx context.Context, orderID string, payment *providers.ERPOrderPayment) error {
	f.record("PutPayment:" + orderID)
	if err := f.scriptedFail("UpdateOrderPayment"); err != nil {
		return err
	}
	return nil
}

func (f *scriptedERP) SetOrderSituacao(ctx context.Context, orderID string, situacao int) error {
	f.record(fmt.Sprintf("Situacao:%d:%s", situacao, orderID))
	if err := f.scriptedFail("SetOrderSituacao"); err != nil {
		return err
	}
	return nil
}

func (f *scriptedERP) AddOrderMarker(ctx context.Context, orderID, marker string) error {
	f.record("Marker:" + orderID)
	return nil
}

func (f *scriptedERP) FindOrderIDByMarker(ctx context.Context, marker string) (string, error) {
	f.record("FindByMarker")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markerOrders == nil {
		return "", nil
	}
	return f.markerOrders[marker], nil
}

func (f *scriptedERP) ReverseOrderStock(ctx context.Context, orderID string) error {
	f.record("OrderReverse:" + orderID)
	if err := f.scriptedFail("ReverseOrderStock"); err != nil {
		return err
	}
	return nil
}

func cartOrderState(t *testing.T, cartID string) (state string, launched bool, orderID string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT erp_order_state, erp_stock_launched, COALESCE(external_order_id,'') FROM carts WHERE id=$1`, cartID).
		Scan(&state, &launched, &orderID); err != nil {
		t.Fatalf("lendo erp_order_state: %v", err)
	}
	return
}

func TestERPOrderConversionHappyPath(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	state, launched, orderID := cartOrderState(t, fx.cartID)
	if state != "open" || !launched || orderID != "ORD-1" {
		t.Fatalf("state=%q launched=%v orderID=%q", state, launched, orderID)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active = %d", n)
	}
	iCreate, iMarker, iLaunch, iReverse := fake.firstIndex("CreateOrder"), fake.firstIndex("Marker"), fake.firstIndex("Launch"), fake.firstIndex("ReverseRes")
	if !(iCreate < iMarker && iMarker < iLaunch && iLaunch < iReverse) {
		t.Fatalf("ordem da conversão violada (create=%d marker=%d launch=%d reverse=%d): %v",
			iCreate, iMarker, iLaunch, iReverse, fake.calls)
	}
	if fake.count("PutPayment") != 0 || fake.count("Situacao") != 0 {
		t.Fatalf("conversão não grava pagamento/situação: %v", fake.calls)
	}
}

func TestERPOrderConversionSingleFlight(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.createDelay = 300 * time.Millisecond
	svc := newFinalisationService(fake)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID)
		}()
	}
	wg.Wait()
	if fake.count("CreateOrder") != 1 {
		t.Fatalf("conversão dupla: %v", fake.calls)
	}
}

func TestERPOrderConversionIdempotent(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	before := len(fake.calls)
	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if len(fake.calls) != before {
		t.Fatalf("segunda chamada tocou o ERP: %v", fake.calls[before:])
	}
}

// O assert de ouro do design C: pagamento confirmado = 2 escritas (parcelas +
// situação), ZERO movimentação de estoque, zero entrada E.
func TestERPOrderConfirmTwoPutsZeroStock(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	launchesBefore, reversalsBefore := fake.count("Launch"), fake.count("ReverseRes")

	if err := svc.ConfirmERPOrderPayment(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if fake.count("PutPayment") != 1 || fake.count("Situacao:3") != 1 {
		t.Fatalf("confirm deveria ser 2 PUTs: %v", fake.calls)
	}
	if fake.count("Launch") != launchesBefore || fake.count("ReverseRes") != reversalsBefore || fake.count("OrderReverse") != 0 {
		t.Fatalf("confirm moveu estoque: %v", fake.calls)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "confirmed" {
		t.Fatalf("state = %q", state)
	}
	finStatus, _, _, _, _ := cartFinalisationState(t, fx.cartID)
	if finStatus != "done" {
		t.Fatalf("erp_finalisation_status = %q (guards/telemetria dependem de done)", finStatus)
	}
}

func TestERPOrderConfirmNotConvertedFallsBack(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	svc := newFinalisationService(newScriptedERP())

	err := svc.ConfirmERPOrderPayment(context.Background(), fx.cartID, fx.storeID, testPaymentStatus())
	if err == nil || !strings.Contains(err.Error(), "não convertido") {
		t.Fatalf("esperava ErrCartNotConverted, veio: %v", err)
	}
}

func TestERPOrderConversionLaunchFallback(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.failures["LaunchOrderStock"] = 1
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("fallback deveria concluir: %v", err)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "open" {
		t.Fatalf("state = %q", state)
	}
	if fake.count("Launch") != 2 {
		t.Fatalf("esperava launch falho + re-launch: %v", fake.calls)
	}
	if fake.firstIndex("ReverseRes") < fake.firstIndex("Launch") {
		t.Fatalf("estorno só depois do launch falhar: %v", fake.calls)
	}
}

func TestERPOrderMutationCycle(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// comprador reduziu a quantidade no checkout
	if _, err := testPool.Exec(context.Background(),
		`UPDATE cart_items SET quantity = 1 WHERE cart_id = $1`, fx.cartID); err != nil {
		t.Fatalf("update qty: %v", err)
	}
	if err := svc.MutateERPOrderItems(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	iRev, iPut := fake.firstIndex("OrderReverse"), fake.firstIndex("PutItens")
	if !(iRev >= 0 && iPut > iRev) {
		t.Fatalf("ciclo estornar→PUT violado: %v", fake.calls)
	}
	if fake.count("Launch") != 2 { // 1 conversão + 1 relançamento do ciclo
		t.Fatalf("launches = %d: %v", fake.count("Launch"), fake.calls)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "open" {
		t.Fatalf("state pós-mutação = %q", state)
	}
}

func TestERPOrderConfirmAdoptsOrphanByMarker(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	fake.markerOrders = map[string]string{"lc-cart-" + fx.cartID: "ORD-77"}
	svc := newFinalisationService(fake)

	// Simula crash pós-create/pré-persistência: converting sem external_order_id.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET erp_order_state = 'converting', erp_op_started_at = now() WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed converting: %v", err)
	}

	if err := svc.ConfirmERPOrderPayment(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirm com adoção: %v", err)
	}
	state, _, orderID := cartOrderState(t, fx.cartID)
	if state != "confirmed" || orderID != "ORD-77" {
		t.Fatalf("state=%q orderID=%q", state, orderID)
	}
	if fake.count("CreateOrder") != 0 {
		t.Fatalf("adoção não pode criar pedido novo: %v", fake.calls)
	}
	if fake.count("FindByMarker") != 1 || fake.count("Launch:ORD-77") != 1 {
		t.Fatalf("fluxo de adoção errado: %v", fake.calls)
	}
}

func TestERPOrderConfirmUnsticksMutating(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// mutação presa (processo morreu no meio do ciclo)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET erp_order_state = 'mutating', erp_op_started_at = now() WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed mutating: %v", err)
	}

	if err := svc.ConfirmERPOrderPayment(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirm reconciliando: %v", err)
	}
	// Reconciliação de grade (OrderReverse+PutItens+Launch) ANTES das parcelas.
	if fake.count("OrderReverse") != 1 || fake.count("PutItens") != 1 {
		t.Fatalf("reconciliação de grade não rodou: %v", fake.calls)
	}
	if fake.firstIndex("PutPayment") < fake.firstIndex("PutItens") {
		t.Fatalf("parcelas antes da reconciliação (invariante violada): %v", fake.calls)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "confirmed" {
		t.Fatalf("state = %q", state)
	}
}

func TestERPOrderCancelReturnsStock(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := svc.CancelERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Ordem validada em sandbox (T7 + corrida C4): situação 2 e SÓ ENTÃO estorno.
	iSit, iRev := fake.firstIndex("Situacao:2"), fake.firstIndex("OrderReverse")
	if !(iSit >= 0 && iRev > iSit) {
		t.Fatalf("ordem cancelar→estornar violada: %v", fake.calls)
	}
	state, launched, _ := cartOrderState(t, fx.cartID)
	if state != "cancelled" || launched {
		t.Fatalf("state=%q launched=%v", state, launched)
	}
	// idempotente
	if err := svc.CancelERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("cancel 2×: %v", err)
	}
}

// ============================================================================
// Wiring do design C — roteamento dos fluxos existentes para o pedido
// ============================================================================

// Mutação de checkout em cart convertido roteia para o ciclo do pedido — sem
// movimentação manual, sem movementID.
func TestAdjustDeltaRoutesToOrderCycle(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// comprador reduz 2→1 (o checkout grava o item ANTES de chamar o adjust)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE cart_items SET quantity = 1 WHERE cart_id = $1`, fx.cartID); err != nil {
		t.Fatalf("update qty: %v", err)
	}
	manualBefore := fake.count("ReverseRes") + fake.count("ReReserve")

	movementID, err := svc.AdjustStockReservationDelta(context.Background(),
		fx.storeID, fx.cartID, fx.eventID, fx.productID, -1, 1000, "@buyer")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if movementID != "" {
		t.Fatalf("modo pedido não gera movementID manual (veio %q)", movementID)
	}
	if fake.count("OrderReverse") != 1 || fake.count("PutItens") != 1 {
		t.Fatalf("ciclo do pedido não rodou: %v", fake.calls)
	}
	if fake.count("ReverseRes")+fake.count("ReReserve") != manualBefore {
		t.Fatalf("movimentação manual vazou no modo pedido: %v", fake.calls)
	}
}

// Live-add / promoção de waitlist em cart convertido: a peça nova entra pelo
// pedido, nunca por saída manual.
func TestReserveStockRoutesToOrderCycle(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// comprador comenta mais 1 durante a live (AddToCart já gravou o item)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE cart_items SET quantity = 2 WHERE cart_id = $1`, fx.cartID); err != nil {
		t.Fatalf("update qty: %v", err)
	}
	manualReserves := fake.count("ReReserve")

	if err := svc.ReserveStockInERP(context.Background(),
		fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, "@buyer"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if fake.count("ReReserve") != manualReserves {
		t.Fatalf("saída manual vazou para cart convertido: %v", fake.calls)
	}
	if fake.count("PutItens") != 1 {
		t.Fatalf("grade não foi aplicada ao pedido: %v", fake.calls)
	}
}

// A entrada única do caminho pago prefere o confirm (2 PUTs) e nunca roda a
// finalização legada em cart convertido.
func TestFinalizeOrConfirmPrefersConfirm(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := svc.finalizeOrConfirmCartERP(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("finalizeOrConfirm: %v", err)
	}
	if fake.count("CreateOrder") != 1 { // só o da conversão
		t.Fatalf("finalização legada rodou em cart convertido: %v", fake.calls)
	}
	if fake.count("PutPayment") != 1 || fake.count("Situacao:3") != 1 {
		t.Fatalf("confirm não rodou: %v", fake.calls)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "confirmed" {
		t.Fatalf("state = %q", state)
	}
}

// Cart convertido que expira sem pagar: o pedido é cancelado e o estoque
// devolvido (situação 2 → estorno) pelo fluxo de expiração existente.
func TestExpiredConvertedCartCancelsOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// pix abandonado: cart ativo, não pago, expirado
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET status = 'active', payment_status = 'pending', paid_at = NULL,
		        expires_at = now() - interval '1 hour' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed expiry: %v", err)
	}

	svc.ProcessExpiredCartsForProduct(context.Background(), fx.eventID, fx.productID)

	iSit, iRev := fake.firstIndex("Situacao:2"), fake.firstIndex("OrderReverse")
	if !(iSit >= 0 && iRev > iSit) {
		t.Fatalf("expiração não cancelou o pedido (ordem cancelar→estornar): %v", fake.calls)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "cancelled" {
		t.Fatalf("state = %q", state)
	}
	var cartStatus string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM carts WHERE id = $1`, fx.cartID).Scan(&cartStatus); err != nil || cartStatus != "expired" {
		t.Fatalf("cart status = %q (err=%v)", cartStatus, err)
	}
}

// ============================================================================
// Sweep, refund, guard composto e health-check de entrega de webhook
// ============================================================================

func TestSweepFinishesStuckConversion(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	// Conversão que morreu após criar o pedido: converting + order gravado,
	// op velha, reserva manual ainda active.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET erp_order_state = 'converting', external_order_id = 'ORD-SW1',
		        erp_op_started_at = now() - interval '30 minutes' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed stuck: %v", err)
	}

	svc.RunERPOrderOpsSweep(context.Background())

	state, launched, orderID := cartOrderState(t, fx.cartID)
	if state != "open" || !launched || orderID != "ORD-SW1" {
		t.Fatalf("sweep não terminou a conversão: state=%q launched=%v order=%q", state, launched, orderID)
	}
	if fake.count("Launch:ORD-SW1") != 1 || fake.count("CreateOrder") != 0 {
		t.Fatalf("sweep deve lançar sem criar pedido novo: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("sweep não estornou a reserva (n=%d)", n)
	}
}

func TestSweepAdoptsOrphanConversionByMarker(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	fake.markerOrders = map[string]string{"lc-cart-" + fx.cartID: "ORD-SW2"}
	svc := newFinalisationService(fake)

	// Conversão que morreu ENTRE o POST e a persistência do id.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET erp_order_state = 'converting',
		        erp_op_started_at = now() - interval '30 minutes' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	svc.RunERPOrderOpsSweep(context.Background())

	state, _, orderID := cartOrderState(t, fx.cartID)
	if state != "open" || orderID != "ORD-SW2" {
		t.Fatalf("sweep não adotou o órfão: state=%q order=%q", state, orderID)
	}
	if fake.count("FindByMarker") != 1 || fake.count("CreateOrder") != 0 {
		t.Fatalf("adoção errada: %v", fake.calls)
	}
}

func TestRefundConvertedCartOrderReturnsStock(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := svc.ConfirmERPOrderPayment(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := svc.RefundConvertedCartOrder(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("refund: %v", err)
	}
	iSit, iRev := fake.firstIndex("Situacao:2"), fake.firstIndex("OrderReverse")
	if !(iSit >= 0 && iRev > iSit) {
		t.Fatalf("refund deve cancelar e SÓ ENTÃO estornar: %v", fake.calls)
	}
	state, _, _ := cartOrderState(t, fx.cartID)
	if state != "cancelled" {
		t.Fatalf("state pós-refund = %q", state)
	}
	// Idempotente: segundo refund é no-op (estado já cancelled).
	callsBefore := len(fake.calls)
	if err := svc.RefundConvertedCartOrder(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("refund 2×: %v", err)
	}
	if len(fake.calls) != callsBefore {
		t.Fatalf("refund duplo tocou o ERP: %v", fake.calls[callsBefore:])
	}
}

// Guard composto: conversão/mutação em voo arma a supressão de sync/promoção
// mesmo sem cart pago.
func TestStockGuardArmsDuringConversion(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET payment_status = 'pending', paid_at = NULL,
		        erp_order_state = 'converting', erp_op_started_at = now() WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("seed converting: %v", err)
	}
	ext := extProductID(t, fx.productID)

	guarded, err := testRepo.HasStockGuardForProduct(ctx, ext, fx.storeID, "tiny")
	if err != nil || !guarded {
		t.Fatalf("guard deveria armar durante converting (guarded=%v err=%v)", guarded, err)
	}
	inFlight, err := testRepo.HasInFlightFinalisationForProduct(ctx, fx.productID)
	if err != nil || !inFlight {
		t.Fatalf("in-flight deveria armar durante converting (err=%v)", err)
	}

	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET erp_order_state = 'open' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("move to open: %v", err)
	}
	guarded, err = testRepo.HasStockGuardForProduct(ctx, ext, fx.storeID, "tiny")
	if err != nil {
		t.Fatalf("guard pós-open: %v", err)
	}
	if guarded {
		t.Fatal("guard deve DESARMAR em open (estado estável, sem ciclo em voo)")
	}
}

// Health-check de entrega: integração ativa sem eventos de estoque na janela
// é listada uma vez (dedupe de 24h via metadata).
func TestStaleStockWebhookDetection(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	ctx := context.Background()

	var integrationID string
	if err := testPool.QueryRow(ctx,
		`UPDATE integrations SET created_at = now() - interval '2 days'
		 WHERE store_id = $1 AND provider = 'tiny' RETURNING id::text`, fx.storeID).Scan(&integrationID); err != nil {
		t.Fatalf("envelhecendo integração: %v", err)
	}

	listedIDs := func() map[string]bool {
		rows, err := testRepo.ListTinyIntegrationsWithStaleStockWebhook(ctx, 12*time.Hour)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.IntegrationID] = true
		}
		return out
	}

	if !listedIDs()[integrationID] {
		t.Fatal("integração sem eventos deveria ser listada")
	}
	// Dedupe: após o carimbo, some por 24h.
	if err := testRepo.StampIntegrationStockWebhookAlert(ctx, integrationID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if listedIDs()[integrationID] {
		t.Fatal("integração carimbada não deveria repetir o alerta")
	}
	// Evento recente também silencia (independente do carimbo).
	if _, err := testPool.Exec(ctx,
		`UPDATE integrations SET metadata = metadata - 'stock_webhook_alerted_at' WHERE id = $1::uuid;`, integrationID); err != nil {
		t.Fatalf("limpando carimbo: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO webhook_events (integration_id, provider, event_type, event_id, payload, signature_valid)
		 VALUES ($1::uuid, 'tiny', 'estoque', 'evt-1', '{}'::jsonb, true)`, integrationID); err != nil {
		t.Fatalf("seed evento: %v", err)
	}
	if listedIDs()[integrationID] {
		t.Fatal("integração com evento recente não deveria alertar")
	}
}

// ============================================================================
// RACE CONDITION — reprodução literal da timeline t0-t8 do relatório
// (a promoção fantasma da waitlist que originou toda a refatoração)
// ============================================================================

// Cenário: produto esgotado (estoque local 0), cliente A pagou 1 unidade e sua
// finalização ERP está EM VOO (reserva active, cart paid+pending), cliente B
// esperando na fila. Durante a finalização, a reversão da reserva de A infla o
// saldo do Tiny → webhook tipo=estoque chega. SEM o guard, o backstop promovia
// B contra uma unidade que não existe (DM irreversível + Tiny negativo). COM o
// guard, a promoção é ADIADA enquanto a finalização de A está em voo.
func TestRaceWaitlistPhantomPromotionBlockedDuringFinalisation(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 1) // cart A: paid+pending, 1 reserva active
	ext := extProductID(t, fx.productID)

	// Produto esgotado: estoque local 0 (A já consumiu na live).
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 0 WHERE id = $1`, fx.productID); err != nil {
		t.Fatalf("zerando estoque: %v", err)
	}
	// Cliente B na fila para o MESMO produto/evento.
	seedWaitingBuyer(t, fx, "B")

	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	svc.productSyncer = raceStubالسyncer{ext: ext}

	// t3: webhook de estoque chega COM a finalização de A em voo (paid+pending).
	if err := svc.ProcessWaitlistAfterStockWebhook(ctx, fx.storeID, "tiny", ext); err != nil {
		t.Fatalf("backstop: %v", err)
	}

	// B NÃO pode ter sido promovido — guard armado.
	if s := waitlistStatus(t, fx.eventID, fx.productID); s != "waiting" {
		t.Fatalf("PROMOÇÃO FANTASMA: B saiu de 'waiting' para %q com finalização em voo", s)
	}
	if st := localStock(t, fx.productID); st != 0 {
		t.Fatalf("estoque local mexeu (=%d) — guard não segurou", st)
	}
	if fake.count("ReReserve") != 0 {
		t.Fatalf("saída manual criada para B durante o race: %v", fake.calls)
	}

	// t8+: finalização de A conclui (done) → o guard desarma → o PRÓXIMO
	// webhook promove B legitimamente (agora com estoque real devolvido).
	if err := testRepo.MarkCartERPFinalisationDone(ctx, fx.cartID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 1 WHERE id = $1`, fx.productID); err != nil {
		t.Fatalf("devolvendo 1 unidade real: %v", err)
	}
	if err := svc.ProcessWaitlistAfterStockWebhook(ctx, fx.storeID, "tiny", ext); err != nil {
		t.Fatalf("backstop pós-done: %v", err)
	}
	if s := waitlistStatus(t, fx.eventID, fx.productID); s != "notified" {
		t.Fatalf("B deveria ser promovido após o guard desarmar (status=%q)", s)
	}
}

// Controle negativo: sem finalização em voo e com estoque real, a promoção
// acontece — prova que o guard bloqueia SÓ a janela perigosa, não sempre.
func TestRaceWaitlistPromotesWhenStockGenuinelyFree(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	// Cart A já finalizado (não arma guard) e estoque real devolvido.
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET erp_finalisation_status = 'done', payment_status = 'paid' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("marcando A done: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 1 WHERE id = $1`, fx.productID); err != nil {
		t.Fatalf("estoque: %v", err)
	}
	seedWaitingBuyer(t, fx, "B")

	svc := newFinalisationService(newScriptedERP())
	svc.productSyncer = raceStubالسyncer{ext: ext}

	if err := svc.ProcessWaitlistAfterStockWebhook(ctx, fx.storeID, "tiny", ext); err != nil {
		t.Fatalf("backstop: %v", err)
	}
	if s := waitlistStatus(t, fx.eventID, fx.productID); s != "notified" {
		t.Fatalf("B deveria ser promovido com estoque livre (status=%q)", s)
	}
	if st := localStock(t, fx.productID); st != 0 {
		t.Fatalf("promoção deveria consumir a unidade (estoque=%d)", st)
	}
}

// Concorrência real: dois webhooks de estoque batendo ao mesmo tempo com UMA
// unidade livre e DOIS clientes na fila — o gate atômico DecrementProductStock
// garante que só um é promovido (sem over-promote).
func TestRaceConcurrentWebhooksSinglePromotion(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET erp_finalisation_status = 'done' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("done: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 1 WHERE id = $1`, fx.productID); err != nil {
		t.Fatalf("estoque: %v", err)
	}
	seedWaitingBuyer(t, fx, "B")
	seedWaitingBuyer(t, fx, "C")

	svc := newFinalisationService(newScriptedERP())
	svc.productSyncer = raceStubالسyncer{ext: ext}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.ProcessWaitlistAfterStockWebhook(ctx, fx.storeID, "tiny", ext)
		}()
	}
	wg.Wait()

	var notified int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM waitlist_items WHERE event_id = $1 AND product_id = $2 AND status = 'notified'`,
		fx.eventID, fx.productID).Scan(&notified); err != nil {
		t.Fatalf("contando notified: %v", err)
	}
	if notified != 1 {
		t.Fatalf("over-promote: %d clientes promovidos para 1 unidade", notified)
	}
	if st := localStock(t, fx.productID); st != 0 {
		t.Fatalf("estoque final = %d (esperado 0)", st)
	}
}

// --- helpers do race ---

type raceStubالسyncer struct{ ext string }

func (s raceStubالسyncer) HasProduct(ctx context.Context, storeID, externalID, externalSource string) (bool, error) {
	return true, nil
}
func (s raceStubالسyncer) GetProduct(ctx context.Context, storeID, productID string) (string, string, error) {
	return s.ext, "tiny", nil
}
func (s raceStubالسyncer) FilterRegisteredExternalIDs(ctx context.Context, storeID, externalSource string, externalIDs []string) ([]string, error) {
	return nil, nil
}
func (s raceStubالسyncer) SyncProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct, skipStock, downgradeOnly bool) error {
	return nil
}
func (s raceStubالسyncer) ImportProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct) (string, error) {
	return "", nil
}

func seedWaitingBuyer(t *testing.T, fx finFixture, tag string) {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("%s-%d", tag, seedSeq) // único entre testes (DB compartilhado)
	var pos int
	_ = testPool.QueryRow(ctx,
		`SELECT COALESCE(MAX(position),0)+1 FROM waitlist_items WHERE event_id=$1 AND product_id=$2`,
		fx.eventID, fx.productID).Scan(&pos)
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'user-'||$2, '@wl'||$2, 'tokwl-'||$2, (floor(random()*90000)+1)::int, 'checkout', 'unpaid')
		 RETURNING id::text`, fx.eventID, uniq).Scan(&cartID); err != nil {
		t.Fatalf("seed cart B/%s: %v", tag, err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 1, 1000, 1)`, cartID, fx.productID); err != nil {
		t.Fatalf("seed cart_item wl: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, cart_id, status)
		 VALUES ($1, $2, 'user-'||$3, '@wl'||$3, 1, $4, $5, 'waiting')`,
		fx.eventID, fx.productID, uniq, pos, cartID); err != nil {
		t.Fatalf("seed waitlist_item: %v", err)
	}
}

func waitlistStatus(t *testing.T, eventID, productID string) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM waitlist_items WHERE event_id=$1 AND product_id=$2 ORDER BY position ASC LIMIT 1`,
		eventID, productID).Scan(&s); err != nil {
		t.Fatalf("lendo waitlist status: %v", err)
	}
	return s
}

func localStock(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id=$1`, productID).Scan(&n); err != nil {
		t.Fatalf("lendo stock: %v", err)
	}
	return n
}

// ============================================================================
// Guard downgrade-only: redução do lojista reflete, aumento é ignorado
// (correção do bug: estoque do Tiny não refletia durante a live)
// ============================================================================

func realProductSyncer() ProductSyncer {
	return product.NewProductSyncerAdapter(
		product.NewService(product.NewRepository(sqlc.New(testPool), testPool), zap.NewNop()),
	)
}

func productStockByID(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id=$1`, productID).Scan(&n); err != nil {
		t.Fatalf("lendo stock: %v", err)
	}
	return n
}

func erpProd(ext string, stock int) providers.ERPProduct {
	return providers.ERPProduct{ID: ext, Name: "Produto Teste", Price: 1000, Stock: stock, Active: true}
}

// Na janela do guard: uma REDUÇÃO do lojista no Tiny deve refletir no local.
func TestGuardDowngradeOnlyAppliesReduction(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 10 WHERE id=$1`, fx.productID); err != nil {
		t.Fatalf("seed stock: %v", err)
	}
	syncer := realProductSyncer()

	// downgradeOnly=true (guard armado), ERP reporta 4 (< 10) → aplica.
	if err := syncer.SyncProduct(ctx, fx.storeID, "tiny", erpProd(ext, 4), false, true); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := productStockByID(t, fx.productID); got != 4 {
		t.Fatalf("redução do lojista deveria refletir (stock=%d, esperado 4)", got)
	}
}

// Na janela do guard: um AUMENTO (eco de reserva / inflação de finalização)
// deve ser IGNORADO — é o que evita a promoção fantasma.
func TestGuardDowngradeOnlyIgnoresIncrease(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 5 WHERE id=$1`, fx.productID); err != nil {
		t.Fatalf("seed stock: %v", err)
	}
	syncer := realProductSyncer()

	// downgradeOnly=true, ERP reporta 9 (> 5) → preserva o local.
	if err := syncer.SyncProduct(ctx, fx.storeID, "tiny", erpProd(ext, 9), false, true); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := productStockByID(t, fx.productID); got != 5 {
		t.Fatalf("aumento na janela do guard deveria ser ignorado (stock=%d, esperado 5)", got)
	}
}

// Fora da janela do guard (sync normal): aplica qualquer valor.
func TestNormalSyncAppliesAnyValue(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 5 WHERE id=$1`, fx.productID); err != nil {
		t.Fatalf("seed stock: %v", err)
	}
	syncer := realProductSyncer()

	if err := syncer.SyncProduct(ctx, fx.storeID, "tiny", erpProd(ext, 12), false, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := productStockByID(t, fx.productID); got != 12 {
		t.Fatalf("sync normal deveria aplicar o valor do ERP (stock=%d, esperado 12)", got)
	}
}

// ============================================================================
// Promoção PARCIAL da fila (bug do teste real: fila de 3, libera 1 unidade)
// ============================================================================

func waitlistQtyAndStatus(t *testing.T, eventID, productID string) (qty int, status string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT quantity, status FROM waitlist_items WHERE event_id=$1 AND product_id=$2 ORDER BY position ASC LIMIT 1`,
		eventID, productID).Scan(&qty, &status); err != nil {
		t.Fatalf("lendo waitlist: %v", err)
	}
	return
}

func cartItemAvailable(t *testing.T, cartID, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT (quantity - waitlisted_quantity) FROM cart_items WHERE cart_id=$1 AND product_id=$2`,
		cartID, productID).Scan(&n); err != nil {
		t.Fatalf("lendo cart_item: %v", err)
	}
	return n
}

// Reprodução do bug: um cliente na fila pediu 3, apenas 1 unidade libera.
// Antes: promoção all-or-nothing falhava e a unidade ficava órfã.
// Agora: promoção parcial — cliente recebe 1, continua na fila por 2.
func TestWaitlistPartialPromotion(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	// produto esgotado, cliente na fila pedindo 3
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 0 WHERE id=$1`, fx.productID); err != nil {
		t.Fatalf("zerar estoque: %v", err)
	}
	wlCart := seedQueueWaiterQty(t, fx, fx.productID, 1, 3) // fila: pos 1, qtd 3

	svc := newFinalisationService(newScriptedERP())
	svc.productSyncer = raceStubالسyncer{ext: ext}

	// 1 unidade libera (ex.: outro cliente removeu 1 do carrinho)
	if err := testRepo.IncrementProductStock(ctx, fx.productID, 1); err != nil {
		t.Fatalf("liberar 1: %v", err)
	}
	svc.ProcessWaitlistForProduct(ctx, fx.eventID, fx.productID, fx.storeID)

	// O cliente da fila deve ter recebido 1, e continuar na fila por 2.
	qty, status := waitlistQtyAndStatus(t, fx.eventID, fx.productID)
	if status != "waiting" || qty != 2 {
		t.Fatalf("PARCIAL falhou: status=%q qtd_restante=%d (esperado waiting/2)", status, qty)
	}
	if avail := cartItemAvailable(t, wlCart, fx.productID); avail != 1 {
		t.Fatalf("cliente deveria ter 1 disponível no carrinho (avail=%d)", avail)
	}
	if st := localStock(t, fx.productID); st != 0 {
		t.Fatalf("a unidade liberada deveria ter ido para a fila (stock=%d, esperado 0)", st)
	}

	// Libera mais 2 → completa o pedido; agora vira 'notified'.
	if err := testRepo.IncrementProductStock(ctx, fx.productID, 2); err != nil {
		t.Fatalf("liberar 2: %v", err)
	}
	svc.ProcessWaitlistForProduct(ctx, fx.eventID, fx.productID, fx.storeID)
	qty, status = waitlistQtyAndStatus(t, fx.eventID, fx.productID)
	if status != "notified" {
		t.Fatalf("após completar, deveria ser notified (status=%q qtd=%d)", status, qty)
	}
	if avail := cartItemAvailable(t, wlCart, fx.productID); avail != 3 {
		t.Fatalf("cliente deveria ter os 3 disponíveis (avail=%d)", avail)
	}
}

// Promoção TOTAL num passo continua funcionando (estoque >= pedido).
func TestWaitlistFullPromotionStillWorks(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 0 WHERE id=$1`, fx.productID); err != nil {
		t.Fatalf("zerar: %v", err)
	}
	wlCart := seedQueueWaiterQty(t, fx, fx.productID, 1, 3)
	svc := newFinalisationService(newScriptedERP())
	svc.productSyncer = raceStubالسyncer{ext: ext}

	if err := testRepo.IncrementProductStock(ctx, fx.productID, 5); err != nil { // sobra estoque
		t.Fatalf("liberar 5: %v", err)
	}
	svc.ProcessWaitlistForProduct(ctx, fx.eventID, fx.productID, fx.storeID)

	_, status := waitlistQtyAndStatus(t, fx.eventID, fx.productID)
	if status != "notified" {
		t.Fatalf("promoção total deveria virar notified (status=%q)", status)
	}
	if avail := cartItemAvailable(t, wlCart, fx.productID); avail != 3 {
		t.Fatalf("cliente deveria ter 3 disponíveis (avail=%d)", avail)
	}
	if st := localStock(t, fx.productID); st != 2 {
		t.Fatalf("estoque restante deveria ser 2 (st=%d)", st)
	}
}

// Sem estoque, o cliente permanece esperando com a quantidade intacta.
func TestWaitlistNoStockKeepsWaiting(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	ext := extProductID(t, fx.productID)
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock = 0 WHERE id=$1`, fx.productID); err != nil {
		t.Fatalf("zerar: %v", err)
	}
	seedQueueWaiterQty(t, fx, fx.productID, 1, 3)
	svc := newFinalisationService(newScriptedERP())
	svc.productSyncer = raceStubالسyncer{ext: ext}

	svc.ProcessWaitlistForProduct(ctx, fx.eventID, fx.productID, fx.storeID)
	qty, status := waitlistQtyAndStatus(t, fx.eventID, fx.productID)
	if status != "waiting" || qty != 3 {
		t.Fatalf("sem estoque deveria manter waiting/3 (status=%q qtd=%d)", status, qty)
	}
}

// seedQueueWaiterQty: como seedWaitingBuyer mas com quantidade explícita.
func seedQueueWaiterQty(t *testing.T, fx finFixture, productID string, position, qty int) string {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("wq%d-%d", position, seedSeq)
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-'||$2, '@'||$2, 'tk-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid')
		 RETURNING id::text`, fx.eventID, uniq).Scan(&cartID); err != nil {
		t.Fatalf("seed waiter cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, 1000, $3)`, cartID, productID, qty); err != nil {
		t.Fatalf("seed cart_item: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, cart_id, status)
		 VALUES ($1, $2, 'u-'||$3, '@'||$3, $4, $5, $6, 'waiting')`,
		fx.eventID, productID, uniq, qty, position, cartID); err != nil {
		t.Fatalf("seed waitlist_item: %v", err)
	}
	return cartID
}

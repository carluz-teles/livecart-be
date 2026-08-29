package integration

// Infraestrutura dos testes de integração do fluxo ERP.
//
// Rodam contra um Postgres REAL com as migrations completas aplicadas num
// database descartável, e um ERP roteirizado (scriptedERP) no lugar do de
// verdade. Os cenários vivem em fluxo_pedido_db_test.go.
//
// Pré-requisito local:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' go test ./apps/api/internal/integration/ -run Finalisation -v
//
// Sem TEST_DATABASE_URL os testes fazem Skip (CI sem banco continua verde).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp/erpwrite"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/database"
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

	// MaxConns EXPLÍCITO, e pequeno de propósito.
	//
	// Sem isto o pgxpool usa max(4, NumCPU), então o limiar de exaustão do pool
	// virava uma propriedade da MÁQUINA: os testes de escala da fila passavam ou
	// deadlockavam conforme o número de núcleos de quem rodava
	// (TestScaleMultiProductParallelCascades, com P=20, só passava em máquina de
	// 20+ CPUs — verde por acidente de hardware). Produção roda com MaxConns=10
	// (lib/database/postgres.go); 8 aqui deixa o teste MAIS apertado que a
	// produção e transforma os cenários de escala num detector honesto do
	// invariante "detentores do advisory lock < MaxConns".
	poolCfg, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parseando config do pool de teste: %v\n", err)
		return 1
	}
	poolCfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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

// seedPaidCart cria loja + integração + evento + produto + carrinho pago, com a
// Order e a order_payments já materializadas (é o que OnCartPaid faz em produção
// antes de o reactor de ERP rodar).
//
// O segundo parâmetro era o número de reservas manuais a semear. Ele continua na
// assinatura para não churnar os chamadores, mas hoje é ignorado: reserva manual
// não existe mais — quem segura a peça é o pedido de venda. Passar qualquer
// número não muda nada.
func seedPaidCart(t *testing.T, qty, _ int) finFixture {
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
		`INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1, 'ended', 'Live Teste', now()) RETURNING id::text`, fx.storeID)
	kw := fmt.Sprintf("%d", 1000+seedSeq%9000) // keyword numérica 1000-9999 (regra do domínio)
	mustScan(&fx.productID,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Produto Teste', 'tiny', 'EXT-'||$2, $3, 1000, 10) RETURNING id::text`, fx.storeID, n, kw)
	mustScan(&fx.cartID,
		`INSERT INTO carts (event_id, store_id, platform_user_id, platform_handle, token, short_id, status, payment_status, paid_at)
		 SELECT $1, e.store_id, 'user-'||$2, '@buyer'||$2, 'tok-'||$2, ($2)::bigint % 100000, 'checkout', 'paid', now()
		 FROM live_events e WHERE e.id = $1 RETURNING id::text`,
		fx.eventID, n)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, 1000, 0)`, fx.cartID, fx.productID, qty); err != nil {
		t.Fatalf("seed cart_items: %v", err)
	}
	// Fatia 11b: a finalização/NF do ERP são autoritativas em order_payments,
	// resolvidas via orders.cart_id. Em produção OnCartPaid materializa a Order
	// (e a order_payments) ANTES do reactor de ERP rodar no mesmo task de
	// cart.paid; aqui materializamos a linha mínima para que as escritas/leituras
	// de finalização (Mark*, Get*, guards) tenham alvo. Sem isso todo UPDATE de
	// finalização seria um no-op silencioso (0 rows) e os guards leriam 'pending'.
	var orderID string
	mustScan(&orderID,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id)
		 SELECT id, short_id, $2::uuid, event_id FROM carts WHERE id = $1
		 RETURNING id::text`, fx.cartID, fx.storeID)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO order_payments (order_id) VALUES ($1::uuid)`, orderID); err != nil {
		t.Fatalf("seed order_payments: %v", err)
	}
	return fx
}

// cartFinalisationState lê o estado de finalização do ERP. Fatia 11b: as colunas
// de finalização/snapshot passaram a ser autoritativas em order_payments
// (resolvidas via orders.cart_id); external_order_id continua no cart (reserva).
func cartFinalisationState(t *testing.T, cartID string) (status, lastError, externalOrderID string, attempts int, hasSnapshot bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT op.erp_finalisation_status, COALESCE(op.erp_last_error,''),
		        COALESCE(c.external_order_id,''),
		        op.erp_attempts_count, op.erp_payment_snapshot IS NOT NULL
		 FROM order_payments op
		 JOIN orders o ON o.id = op.order_id
		 JOIN carts  c ON c.id = o.cart_id
		 WHERE o.cart_id = $1`, cartID).
		Scan(&status, &lastError, &externalOrderID, &attempts, &hasSnapshot)
	if err != nil {
		t.Fatalf("lendo estado de finalização (order_payments): %v", err)
	}
	return
}

// setOrderFinalisationStatus escreve o status de finalização direto na fonte
// autoritativa (order_payments, resolvida via cart_id) — usado pelos seeds que
// antes da Fatia 11b faziam `UPDATE carts SET erp_finalisation_status = ...`.
func setOrderFinalisationStatus(t *testing.T, cartID, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE order_payments op SET erp_finalisation_status = $2
		 FROM orders o WHERE o.id = op.order_id AND o.cart_id = $1`, cartID, status); err != nil {
		t.Fatalf("setOrderFinalisationStatus: %v", err)
	}
}

// activeReservationCount conta reservas manuais do carrinho. Existe para os
// testes afirmarem que ela é SEMPRE zero: nada no sistema cria uma dessas hoje, e
// um dia em que voltasse a criar seria uma regressão silenciosa — a saída manual
// baixa o físico e realimenta o webhook de estoque na nossa própria fila.
func activeReservationCount(t *testing.T, cartID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM stock_reservations WHERE cart_id = $1 AND status = 'active'`, cartID).Scan(&n); err != nil {
		t.Fatalf("contando reservas: %v", err)
	}
	return n
}

// cartERPState lê o estado do pedido-como-reserva no carrinho.
func cartERPState(t *testing.T, cartID string) (state, externalOrderID, statusERP string, launched bool) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT erp_order_state, COALESCE(external_order_id,''),
		        COALESCE(erp_order_status,''), erp_stock_launched
		 FROM carts WHERE id = $1`, cartID).
		Scan(&state, &externalOrderID, &statusERP, &launched); err != nil {
		t.Fatalf("lendo estado do pedido no carrinho: %v", err)
	}
	return
}

// erpStatusHistory devolve o trajeto gravado do pedido, do mais antigo para o
// mais novo.
func erpStatusHistory(t *testing.T, cartID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT status FROM erp_order_status_events WHERE cart_id = $1 ORDER BY observed_at, id`, cartID)
	if err != nil {
		t.Fatalf("lendo histórico de situação: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan do histórico: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// ============================================================================
// ERP roteirizado
// ============================================================================

type scriptedERP struct {
	providers.ERPProvider // nil: método não roteirizado = panic (chamada inesperada)

	mu             sync.Mutex
	calls          []string
	failures       map[string]int // método -> próximas chamadas falham com erro AMBÍGUO
	provenFailures map[string]int // método -> próximas chamadas falham com prova de não-entrega
	createDelay    time.Duration
	orderSeq       int
	markerOrders   map[string]string // marcador -> orderID (FindOrderIDByMarker)
	prefixo        string            // isola os ids de pedido deste roteiro
	situacoes      map[string]int    // pedido -> situação que o GET devolve
	// bloqueiaProximosPuts faz os N próximos PUT /itens recusarem por estoque
	// lançado — é como o ERP real anuncia que alguém lançou pelo painel.
	bloqueiaProximosPuts int
	ultimaGrade          []providers.ERPOrderItem
}

// erpSeq dá a cada ERP roteirizado um prefixo próprio de id de pedido.
//
// Sem isso todos geravam "ORD-1", e como o database de teste é compartilhado
// pela execução inteira, carrinhos de cenários diferentes ficavam com o MESMO
// external_order_id — e uma asserção sobre "este pedido" passava a contar os dos
// outros.
var erpSeq atomic.Int64

func newScriptedERP() *scriptedERP {
	return &scriptedERP{
		prefixo:        fmt.Sprintf("ORD%d", erpSeq.Add(1)),
		failures:       map[string]int{},
		provenFailures: map[string]int{},
		markerOrders:   map[string]string{},
		situacoes:      map[string]int{},
	}
}

func (f *scriptedERP) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *scriptedERP) scriptedFail(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provenFailures[name] > 0 {
		f.provenFailures[name]--
		// Falha de discagem: comprovadamente não chegou à aplicação do ERP. É
		// a classe que o razão de movimentos re-executa sozinho.
		return fmt.Errorf("scripted dial failure: %s: %w", name,
			errors.Join(providers.ErrProvenUndelivered, errors.New("connection refused")))
	}
	if f.failures[name] > 0 {
		f.failures[name]--
		// Erro genérico: AMBÍGUO para o razão (o ERP pode ter aplicado). Nunca
		// re-executado às cegas.
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
	id := fmt.Sprintf("%s-%d", f.prefixo, f.orderSeq)
	// O carimbo acontece DENTRO da criação, como no provider de verdade: é a
	// única âncora buscável, e um roteiro que não o registrasse esconderia a
	// adoção que ele existe para permitir.
	if order.ExternalID != "" {
		f.markerOrders["lc-cart-"+order.ExternalID] = id
	}
	f.ultimaGrade = append([]providers.ERPOrderItem(nil), order.Items...)
	f.mu.Unlock()
	return &providers.OrderResult{OrderID: id, OrderNumber: id, Status: "created"}, nil
}

// ReverseOrderStock existe no roteiro para que os testes possam AFIRMAR que ele
// nunca é chamado. Num pedido que só reservou, estornar infla a reserva.
func (f *scriptedERP) ReverseOrderStock(ctx context.Context, orderID string) error {
	f.record("Reverse:" + orderID)
	return f.scriptedFail("ReverseOrderStock")
}

func (f *scriptedERP) UpdateOrderItems(ctx context.Context, orderID string, itens []providers.ERPOrderItem) error {
	f.record(fmt.Sprintf("PutItens:%s:%d", orderID, len(itens)))
	f.mu.Lock()
	f.ultimaGrade = append([]providers.ERPOrderItem(nil), itens...)
	bloqueado := f.bloqueiaProximosPuts > 0
	if bloqueado {
		f.bloqueiaProximosPuts--
	}
	f.mu.Unlock() // scriptedFail toma a mesma trava
	if bloqueado {
		return providers.ErrOrderStockLaunched
	}
	return f.scriptedFail("UpdateOrderItems")
}

// GetOrderItems devolve a grade que o roteiro guardou, com as notas — é o que a
// preservação das linhas do lojista lê antes de escrever.
func (f *scriptedERP) GetOrderItems(ctx context.Context, orderID string) ([]providers.ERPOrderItem, error) {
	f.record("GetItens:" + orderID)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]providers.ERPOrderItem, len(f.ultimaGrade))
	copy(out, f.ultimaGrade)
	return out, nil
}

func (f *scriptedERP) UpdateOrderPayment(ctx context.Context, orderID string, _ *providers.ERPOrderPayment) error {
	f.record("Payment:" + orderID)
	return f.scriptedFail("UpdateOrderPayment")
}

func (f *scriptedERP) SetOrderSituacao(ctx context.Context, orderID string, situacao int) error {
	f.record(fmt.Sprintf("Situacao:%s:%d", orderID, situacao))
	return f.scriptedFail("SetOrderSituacao")
}

func (f *scriptedERP) GetOrderSituacao(ctx context.Context, orderID string) (int, error) {
	f.record("GetSituacao:" + orderID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.situacoes[orderID]; ok {
		return s, nil
	}
	return providers.SituacaoAberta, nil
}

func (f *scriptedERP) AddOrderMarker(ctx context.Context, orderID, marker string) error {
	f.record("Marker:" + orderID)
	f.mu.Lock()
	if f.markerOrders == nil {
		f.markerOrders = map[string]string{}
	}
	f.markerOrders[marker] = orderID
	f.mu.Unlock() // scriptedFail toma a mesma trava
	return f.scriptedFail("AddOrderMarker")
}

func (f *scriptedERP) FindOrderIDByMarker(ctx context.Context, marker string) (string, error) {
	f.record("FindMarker:" + marker)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markerOrders[marker], nil
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
	svc := &Service{
		repo:   testRepo,
		logger: zap.NewNop(),
		stock:  NewStockReservations(testRepo, zap.NewNop()),
		erpProviderFactory: func(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
			return fake, nil
		},
	}
	// Teto de escrita aberto. Em produção ele é o medido — 4 por segundo em
	// rajada, 30 por minuto sustentadas — e o limitador tem os seus próprios
	// testes; aqui ele só faria cada cenário esperar a janela de 60 segundos do
	// balde sustentado para provar uma regra de negócio.
	svc.ERP().SetWriteLimits(erpwrite.Limits{
		BurstN: 4096, BurstWindow: time.Millisecond,
		SustainedN: 1 << 20, SustWindow: time.Millisecond,
	})
	return svc
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

// callsWithPrefix devolve as chamadas registradas com o prefixo.
func (f *scriptedERP) callsWithPrefix(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

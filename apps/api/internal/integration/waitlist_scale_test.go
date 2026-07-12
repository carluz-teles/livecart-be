package integration

// Stress da CASCATA DE PROMOÇÃO DA FILA em larga escala e sob concorrência.
//
// Cenário do dono do produto: um produto com 1 unidade, vários clientes na
// fila. Quando quem segura a unidade a solta (remove/reduz/expira), o PRÓXIMO
// da fila precisa recebê-la — e se esse também soltar, o seguinte recebe, e
// assim por diante. A promoção é FIFO por posição e o gate atômico
// DecrementProductStock garante que a mesma unidade nunca vai para dois.
//
// Modelagem fiel do fluxo real: no checkout, RemoveCartItem devolve o estoque
// local (IncrementProductStock) e chama ProcessWaitlistForProduct inline — é
// exatamente o par "release + promote" que estes testes exercem, em escala.
//
// Sem integração Tiny nos seeds: ReserveStockInERP no-opa quando não há
// integração ativa, isolando a lógica de fila para rodar com centenas de
// operações concorrentes de forma rápida e determinística.
//
// Rodar (com -race para pegar corridas reais no gate):
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestScale -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// scaleService: Service mínimo, sem ERP/notification/live — a promoção é
// puramente local (ReserveStockInERP no-opa por falta de integração Tiny).
func scaleService() *Service {
	return &Service{repo: testRepo, logger: zap.NewNop()}
}

// scaleFixture é um evento com N produtos, cada um com 1 unidade "sold out"
// (stock=0) e uma fila de compradores esperando.
type scaleFixture struct {
	storeID string
	eventID string
}

func seedScaleEvent(t *testing.T) scaleFixture {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d-%d", seedSeq, rand.Intn(1_000_000))
	var fx scaleFixture
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Scale', 'scale-'||$1) RETURNING id::text`, n).Scan(&fx.storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, waitlist_notified_ttl_minutes)
		 VALUES ($1, 'active', 'Scale Live', 30) RETURNING id::text`, fx.storeID).Scan(&fx.eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return fx
}

// seedSoldOutProductWithQueue cria um produto com stock local `stock` e
// `numWaiters` clientes na fila (cada um com cart_item totalmente waitlisted,
// pronto para ser promovido). Retorna o productID.
func seedSoldOutProductWithQueue(t *testing.T, fx scaleFixture, stock, numWaiters int) string {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d-%d", seedSeq, rand.Intn(1_000_000))
	kw := fmt.Sprintf("K%03d", seedSeq%1000)
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'P '||$2, 'manual', 'EXT-'||$2, $3, 1000, $4) RETURNING id::text`,
		fx.storeID, n, kw, stock).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	for i := 0; i < numWaiters; i++ {
		seedQueueWaiter(t, fx, productID, i+1)
	}
	return productID
}

func seedQueueWaiter(t *testing.T, fx scaleFixture, productID string, position int) string {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("w%d-%d", position, seedSeq)
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-'||$2, '@'||$2, 'tk-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid')
		 RETURNING id::text`, fx.eventID, uniq).Scan(&cartID); err != nil {
		t.Fatalf("seed waiter cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 1, 1000, 1)`, cartID, productID); err != nil {
		t.Fatalf("seed waiter cart_item: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, cart_id, status)
		 VALUES ($1, $2, 'u-'||$3, '@'||$3, 1, $4, $5, 'waiting')`,
		fx.eventID, productID, uniq, position, cartID); err != nil {
		t.Fatalf("seed waitlist_item: %v", err)
	}
	return cartID
}

func countByStatus(t *testing.T, productID, status string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM waitlist_items WHERE product_id=$1 AND status=$2`, productID, status).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", status, err)
	}
	return n
}

func productStock(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id=$1`, productID).Scan(&n); err != nil {
		t.Fatalf("stock: %v", err)
	}
	return n
}

// unitsHeldAvailable = soma de (quantity - waitlisted_quantity) nos carts do
// produto = quantas unidades estão nas mãos de clientes (promovidas).
func unitsHeldAvailable(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(quantity - waitlisted_quantity),0) FROM cart_items WHERE product_id=$1`, productID).Scan(&n); err != nil {
		t.Fatalf("held: %v", err)
	}
	return n
}

// ============================================================================
// 1. Cascata sequencial: 1 unidade, fila longa, cada um solta e o próximo pega
// ============================================================================

func TestScaleCascadeSingleProductFIFO(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	const N = 60
	// stock=0: a unidade está "vendida" (o holder inicial não está na fila).
	productID := seedSoldOutProductWithQueue(t, fx, 0, N)

	// Ordem FIFO esperada das promoções (positions 1..N).
	promotedOrder := make([]int, 0, N)
	for step := 0; step < N; step++ {
		// Alguém que segurava a unidade REMOVE → devolve 1 ao estoque local.
		if err := testRepo.IncrementProductStock(ctx, productID, 1); err != nil {
			t.Fatalf("release step %d: %v", step, err)
		}
		// O próximo da fila deve receber.
		svc.ProcessWaitlistForProduct(ctx, fx.eventID, productID, fx.storeID)

		if got := countByStatus(t, productID, "notified"); got != step+1 {
			t.Fatalf("passo %d: %d notificados, esperado %d — promoção não seguiu a cascata", step, got, step+1)
		}
		if st := productStock(t, productID); st != 0 {
			t.Fatalf("passo %d: estoque=%d após promoção, esperado 0 (a unidade foi para o próximo)", step, st)
		}
		// Qual posição acabou de ser promovida?
		var pos int
		if err := testPool.QueryRow(ctx,
			`SELECT position FROM waitlist_items WHERE product_id=$1 AND status='notified' ORDER BY notified_at DESC NULLS LAST LIMIT 1`,
			productID).Scan(&pos); err == nil {
			promotedOrder = append(promotedOrder, pos)
		}
	}

	if n := countByStatus(t, productID, "notified"); n != N {
		t.Fatalf("cascata incompleta: %d/%d promovidos", n, N)
	}
	if n := countByStatus(t, productID, "waiting"); n != 0 {
		t.Fatalf("sobrou %d na fila", n)
	}
	// FIFO: as posições promovidas devem ser 1,2,3,...,N em ordem.
	for i, pos := range promotedOrder {
		if pos != i+1 {
			t.Fatalf("ordem FIFO violada no passo %d: promovido position=%d, esperado %d", i, pos, i+1)
		}
	}
	// Além da fila: mais um release sem ninguém esperando deixa a unidade livre.
	if err := testRepo.IncrementProductStock(ctx, productID, 1); err != nil {
		t.Fatalf("release final: %v", err)
	}
	svc.ProcessWaitlistForProduct(ctx, fx.eventID, productID, fx.storeID)
	if st := productStock(t, productID); st != 1 {
		t.Fatalf("sem fila, a unidade deveria ficar livre (estoque=%d)", st)
	}
}

// ============================================================================
// 2. Thundering herd: 1 unidade liberada, 200 promoções concorrentes → 1 só
// ============================================================================

func TestScaleThunderingHerdSinglePromotion(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	const WAITERS = 200
	// stock=1: exatamente UMA unidade livre para uma multidão esperando.
	productID := seedSoldOutProductWithQueue(t, fx, 1, WAITERS)

	var wg sync.WaitGroup
	for i := 0; i < WAITERS; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.ProcessWaitlistForProduct(ctx, fx.eventID, productID, fx.storeID)
		}()
	}
	wg.Wait()

	if got := countByStatus(t, productID, "notified"); got != 1 {
		t.Fatalf("OVER-PROMOTE: %d clientes promovidos para 1 unidade sob %d chamadas concorrentes", got, WAITERS)
	}
	if st := productStock(t, productID); st != 0 {
		t.Fatalf("estoque final=%d, esperado 0", st)
	}
	if held := unitsHeldAvailable(t, productID); held != 1 {
		t.Fatalf("exatamente 1 cliente deveria segurar a unidade (held=%d)", held)
	}
}

// ============================================================================
// 3. Liberação em massa: K unidades, M esperando, concorrência → K promovidos
// ============================================================================

func TestScaleConcurrentReleasesExactCount(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	const M = 300
	const K = 50
	productID := seedSoldOutProductWithQueue(t, fx, 0, M)

	// K releases concorrentes (K clientes removem ao mesmo tempo) intercalados
	// com um enxame de tentativas de promoção.
	var wg sync.WaitGroup
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = testRepo.IncrementProductStock(ctx, productID, 1)
			svc.ProcessWaitlistForProduct(ctx, fx.eventID, productID, fx.storeID)
		}()
	}
	// Enxame extra de promoções sem release — não podem promover nada além do K.
	for i := 0; i < M; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.ProcessWaitlistForProduct(ctx, fx.eventID, productID, fx.storeID)
		}()
	}
	wg.Wait()

	if got := countByStatus(t, productID, "notified"); got != K {
		t.Fatalf("promovidos=%d, esperado exatamente K=%d (liberações reais)", got, K)
	}
	if st := productStock(t, productID); st != 0 {
		t.Fatalf("estoque final=%d, esperado 0 (todas as K unidades consumidas por promoções)", st)
	}
	if held := unitsHeldAvailable(t, productID); held != K {
		t.Fatalf("held=%d, esperado K=%d", held, K)
	}
	if w := countByStatus(t, productID, "waiting"); w != M-K {
		t.Fatalf("waiting=%d, esperado M-K=%d", w, M-K)
	}
}

// ============================================================================
// 4. Cascatas paralelas multi-produto: P produtos, cada um com sua fila
// ============================================================================

func TestScaleMultiProductParallelCascades(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	const P = 20
	const Q = 25

	products := make([]string, P)
	for p := 0; p < P; p++ {
		products[p] = seedSoldOutProductWithQueue(t, fx, 0, Q)
	}

	// Cada produto roda sua cascata completa concorrentemente com os outros.
	var wg sync.WaitGroup
	for p := 0; p < P; p++ {
		wg.Add(1)
		go func(productID string) {
			defer wg.Done()
			for step := 0; step < Q; step++ {
				_ = testRepo.IncrementProductStock(ctx, productID, 1)
				svc.ProcessWaitlistForProduct(ctx, fx.eventID, productID, fx.storeID)
			}
		}(products[p])
	}
	wg.Wait()

	// Cada produto: exatamente Q promovidos, zero sobrando, sem vazamento entre
	// produtos (cada fila é isolada por product_id).
	for p, productID := range products {
		if n := countByStatus(t, productID, "notified"); n != Q {
			t.Fatalf("produto %d: %d/%d promovidos", p, n, Q)
		}
		if n := countByStatus(t, productID, "waiting"); n != 0 {
			t.Fatalf("produto %d: %d ainda na fila", p, n)
		}
		if held := unitsHeldAvailable(t, productID); held != Q {
			t.Fatalf("produto %d: held=%d, esperado Q=%d", p, held, Q)
		}
		if st := productStock(t, productID); st != 0 {
			t.Fatalf("produto %d: estoque=%d, esperado 0", p, st)
		}
	}
}

// ============================================================================
// 5. Caos + conservação: interleaving aleatório de release/promote em massa,
//    invariante global sempre preservada (nada some, nada duplica)
// ============================================================================

func TestScaleChaosConservationInvariant(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	const P = 15
	const Q = 40

	products := make([]string, P)
	for p := 0; p < P; p++ {
		products[p] = seedSoldOutProductWithQueue(t, fx, 0, Q)
	}

	// Cada produto começa com 1 "unidade" circulante (representada pelos
	// releases). Total de releases por produto = Q (todos vão passar pela fila).
	// Interleaving caótico: workers aleatórios disparam release+promote em
	// produtos aleatórios até esgotar o orçamento de releases de cada um.
	var totalReleases int64
	remaining := make([]int64, P)
	for p := range remaining {
		remaining[p] = Q
	}

	const WORKERS = 32
	var wg sync.WaitGroup
	for w := 0; w < WORKERS; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed) + 1))
			for {
				p := rng.Intn(P)
				// reserva um release do produto p, se ainda houver
				if atomic.AddInt64(&remaining[p], -1) < 0 {
					atomic.AddInt64(&remaining[p], 1)
					// checa se TODOS acabaram
					done := true
					for i := range remaining {
						if atomic.LoadInt64(&remaining[i]) > 0 {
							done = false
							break
						}
					}
					if done {
						return
					}
					continue
				}
				_ = testRepo.IncrementProductStock(ctx, products[p], 1)
				svc.ProcessWaitlistForProduct(ctx, fx.eventID, products[p], fx.storeID)
				atomic.AddInt64(&totalReleases, 1)
			}
		}(w)
	}
	wg.Wait()

	if totalReleases != int64(P*Q) {
		t.Fatalf("releases processados=%d, esperado P*Q=%d", totalReleases, P*Q)
	}
	// Invariante de conservação por produto: cada release que encontrou fila
	// virou exatamente 1 promoção; held == promovidos; estoque volta a 0.
	grandNotified := 0
	for p, productID := range products {
		notified := countByStatus(t, productID, "notified")
		held := unitsHeldAvailable(t, productID)
		st := productStock(t, productID)
		if notified != Q {
			t.Fatalf("produto %d: notified=%d, esperado Q=%d", p, notified, Q)
		}
		if held != notified {
			t.Fatalf("produto %d: held=%d != notified=%d (unidade sumiu ou duplicou)", p, held, notified)
		}
		if st != 0 {
			t.Fatalf("produto %d: estoque=%d, esperado 0", p, st)
		}
		grandNotified += notified
	}
	if grandNotified != P*Q {
		t.Fatalf("total promovido=%d, esperado P*Q=%d", grandNotified, P*Q)
	}
}

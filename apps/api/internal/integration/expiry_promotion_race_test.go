package integration

// Corrida EXPIRAÇÃO × PROMOÇÃO DA FILA (branch fix/expiry-promotion-race).
//
// Sem o sweep (expiração 100% via schedule asynq), no finalize do evento o cart
// do HOLDER e o cart do WAITLISTER do mesmo produto ganham o MESMO expires_at →
// duas tasks cart.expire disparam concorrentes. A expiração do holder promove o
// waitlister (de-waitlista o item, re-garante estoque, marca 'notified', estende
// a janela). A task do PRÓPRIO waitlister dispara junto — sem guard ela o expira
// no meio da promoção, deixando um cart notified+expired segurando estoque
// vazado.
//
// GARANTIA (forte): um waitlister que é o PRÓXIMO da fila é SEMPRE promovido
// quando o estoque libera; o carrinho dele NUNCA expira pelo próprio timer
// enquanto ele está 'waiting' na fila ou sendo promovido — só DEPOIS de
// promovido, no prazo estendido, se não pagar.
//
// Estes testes exercem o guard de ExpireCart (cart.sql) + o retry/revert de
// ProcessWaitlistForProduct. Reusam os seeds e helpers de waitlist_scale_test.go
// (seedScaleEvent/seedSoldOutProductWithQueue/seedQueueWaiter/scaleService/…).
//
// Rodar (com -race para a invariante de concorrência):
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestExpiryPromotion -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// Helpers específicos da corrida (os de fila vêm de waitlist_scale_test.go)
// ============================================================================

// seedHolderCart cria o cart do HOLDER: segura `qty` unidades REAIS do produto
// (não-waitlisted), então a expiração dele devolve estoque e dispara a promoção
// do próximo da fila. Cart em 'checkout'/'unpaid' como no finalize.
func seedHolderCart(t *testing.T, fx scaleFixture, productID string, qty int) string {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("h-%d-%d", seedSeq, rand.Intn(1_000_000))
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-'||$2, '@'||$2, 'tk-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid')
		 RETURNING id::text`, fx.eventID, uniq).Scan(&cartID); err != nil {
		t.Fatalf("seed holder cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, 1000, 0)`, cartID, productID, qty); err != nil {
		t.Fatalf("seed holder cart_item: %v", err)
	}
	return cartID
}

// seedNotifiedCart cria um cart JÁ PROMOVIDO: item de-waitlisted (available) e
// waitlist_item 'notified' com a janela wlExpiry; a janela do cart em cartExpiry.
func seedNotifiedCart(t *testing.T, fx scaleFixture, productID string, cartExpiry, wlExpiry time.Time) string {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("n-%d-%d", seedSeq, rand.Intn(1_000_000))
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status, expires_at)
		 VALUES ($1, 'u-'||$2, '@'||$2, 'tk-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid', $3)
		 RETURNING id::text`, fx.eventID, uniq, cartExpiry).Scan(&cartID); err != nil {
		t.Fatalf("seed notified cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, 1, 1000, 0)`, cartID, productID); err != nil {
		t.Fatalf("seed notified cart_item: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, cart_id, status, notified_at, expires_at)
		 VALUES ($1, $2, 'u-'||$3, '@'||$3, 1, 1, $4, 'notified', now(), $5)`,
		fx.eventID, productID, uniq, cartID, wlExpiry); err != nil {
		t.Fatalf("seed notified waitlist_item: %v", err)
	}
	return cartID
}

func setCartExpiresAt(t *testing.T, cartID string, at time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET expires_at = $2 WHERE id = $1`, cartID, at); err != nil {
		t.Fatalf("set cart expires_at: %v", err)
	}
}

func cartStatus(t *testing.T, cartID string) string {
	t.Helper()
	var st string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM carts WHERE id = $1`, cartID).Scan(&st); err != nil {
		t.Fatalf("cart status: %v", err)
	}
	return st
}

// cartExpiresInFuture reporta se a janela do cart está no futuro (foi estendida).
func cartExpiresInFuture(t *testing.T, cartID string) bool {
	t.Helper()
	var future bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT expires_at IS NOT NULL AND expires_at > now() FROM carts WHERE id = $1`, cartID).Scan(&future); err != nil {
		t.Fatalf("cart expires_at future: %v", err)
	}
	return future
}

// waitlistStatusOnCart devolve o status do waitlist_item do cart (só há 1 nestes
// seeds).
func waitlistStatusOnCart(t *testing.T, cartID string) string {
	t.Helper()
	var st string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM waitlist_items WHERE cart_id = $1 ORDER BY position LIMIT 1`, cartID).Scan(&st); err != nil {
		t.Fatalf("waitlist status on cart: %v", err)
	}
	return st
}

// unitsHeldAvailableLive soma as unidades disponíveis (quantity-waitlisted) só de
// carts VIVOS (não expirados/cancelados). Diferente de unitsHeldAvailable, ignora
// o cart_item órfão de um holder já expirado — cujo estoque a expiração já
// devolveu — para a invariante de conservação bater com carts reais.
func unitsHeldAvailableLive(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(ci.quantity - ci.waitlisted_quantity), 0)
		   FROM cart_items ci
		   JOIN carts c ON c.id = ci.cart_id
		  WHERE ci.product_id = $1 AND c.status NOT IN ('expired','cancelled')`,
		productID).Scan(&n); err != nil {
		t.Fatalf("held live: %v", err)
	}
	return n
}

func waitlistedQtyOnCart(t *testing.T, cartID, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT waitlisted_quantity FROM cart_items WHERE cart_id = $1 AND product_id = $2`, cartID, productID).Scan(&n); err != nil {
		t.Fatalf("waitlisted qty: %v", err)
	}
	return n
}

// countNotifiedOnExpiredCartsInWindow conta a corrupção proibida: waitlist_items
// 'notified' com janela AINDA VIGENTE (promoção ativa) presos num cart 'expired'.
// Um 'notified' de janela já vencida num cart expirado é lapse legítimo (o
// cliente não pagou no prazo) e não conta.
func countNotifiedOnExpiredCartsInWindow(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*)
		   FROM waitlist_items wi
		   JOIN carts c ON c.id = wi.cart_id
		  WHERE wi.product_id = $1
		    AND wi.status = 'notified'
		    AND (wi.expires_at IS NULL OR wi.expires_at > now())
		    AND c.status = 'expired'`, productID).Scan(&n); err != nil {
		t.Fatalf("count notified-on-expired: %v", err)
	}
	return n
}

// ============================================================================
// (A) Cart de waitlister 'waiting' com janela vencida NÃO expira pelo timer
// ============================================================================

func TestExpiryPromotionWaitingCartDoesNotSelfExpire(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	// stock=0: unidade "vendida"; 1 cliente na fila ('waiting'), cart 'checkout'.
	productID := seedSoldOutProductWithQueue(t, fx, 0, 1)

	var cartID string
	if err := testPool.QueryRow(ctx,
		`SELECT cart_id::text FROM waitlist_items WHERE product_id = $1 AND status = 'waiting' LIMIT 1`,
		productID).Scan(&cartID); err != nil {
		t.Fatalf("lookup waiter cart: %v", err)
	}
	// Janela já vencida (mesma janela do finalize) — sem o guard, o timer o
	// expiraria enquanto ele está na fila.
	setCartExpiresAt(t, cartID, time.Now().Add(-time.Minute))

	// Caminho real da task: RunScheduledExpiry → (janela no passado) → ExpireCart.
	if err := svc.RunScheduledExpiry(ctx, cartID); err != nil {
		t.Fatalf("RunScheduledExpiry: %v", err)
	}
	// E a chamada direta, para não depender do snapshot.
	svc.ExpireCart(ctx, cartID, fx.storeID)

	if st := cartStatus(t, cartID); st == "expired" {
		t.Fatalf("cart de waitlister 'waiting' foi expirado pelo próprio timer (status=%s) — garantia violada", st)
	}
	if st := waitlistStatusOnCart(t, cartID); st != "waiting" {
		t.Fatalf("item da fila deveria seguir 'waiting', got %s", st)
	}
	if n := countNotifiedOnExpiredCartsInWindow(t, productID); n != 0 {
		t.Fatalf("estado notified+expired detectado (%d)", n)
	}
}

// ============================================================================
// (B) Holder expira → PRÓXIMO 'waiting' é promovido; cart dele NÃO expira
// ============================================================================

func TestExpiryPromotionHolderExpiryPromotesNext(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	// stock=0 + 1 waitlister na fila; holder segura 1 unidade real à parte.
	productID := seedSoldOutProductWithQueue(t, fx, 0, 1)
	holderCart := seedHolderCart(t, fx, productID, 1)

	var waiterCart string
	if err := testPool.QueryRow(ctx,
		`SELECT cart_id::text FROM waitlist_items WHERE product_id = $1 AND status = 'waiting' LIMIT 1`,
		productID).Scan(&waiterCart); err != nil {
		t.Fatalf("lookup waiter cart: %v", err)
	}
	// Ambos com a MESMA janela vencida (o cenário do finalize).
	past := time.Now().Add(-time.Minute)
	setCartExpiresAt(t, holderCart, past)
	setCartExpiresAt(t, waiterCart, past)

	// Expira o holder: devolve 1 unidade e promove o próximo da fila.
	svc.ExpireCart(ctx, holderCart, fx.storeID)

	if st := cartStatus(t, holderCart); st != "expired" {
		t.Fatalf("holder deveria expirar, got %s", st)
	}
	if st := waitlistStatusOnCart(t, waiterCart); st != "notified" {
		t.Fatalf("waitlister deveria ser promovido a 'notified', got %s", st)
	}
	if st := cartStatus(t, waiterCart); st == "expired" {
		t.Fatalf("cart do waitlister promovido NÃO pode estar 'expired'")
	}
	if q := waitlistedQtyOnCart(t, waiterCart, productID); q != 0 {
		t.Fatalf("item deveria estar de-waitlisted (waitlisted=0), got %d", q)
	}
	if !cartExpiresInFuture(t, waiterCart) {
		t.Fatalf("a janela do cart promovido deveria ter sido estendida para o futuro")
	}
	if s := productStock(t, productID); s != 0 {
		t.Fatalf("estoque deveria ser 0 (unidade re-garantida ao promovido), got %d", s)
	}
	if n := countNotifiedOnExpiredCartsInWindow(t, productID); n != 0 {
		t.Fatalf("estado notified+expired detectado (%d)", n)
	}
}

// ============================================================================
// (C) Cart promovido ('notified'): janela futura não expira; janela vencida sim
// ============================================================================

func TestExpiryPromotionNotifiedCartWindowRespected(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()

	t.Run("janela de promoção ainda vigente (cart vencido) não expira", func(t *testing.T) {
		fx := seedScaleEvent(t)
		productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
		// Janela do CART no passado, mas a janela da PROMOÇÃO (wi.expires_at) no
		// futuro: guard (b) deve abster (cobre a sub-janela claim→lock).
		cartID := seedNotifiedCart(t, fx, productID,
			time.Now().Add(-time.Minute), time.Now().Add(20*time.Minute))

		svc.ExpireCart(ctx, cartID, fx.storeID)

		if st := cartStatus(t, cartID); st == "expired" {
			t.Fatalf("cart 'notified' dentro da janela de promoção NÃO pode expirar, got %s", st)
		}
		if n := countNotifiedOnExpiredCartsInWindow(t, productID); n != 0 {
			t.Fatalf("estado notified+expired detectado (%d)", n)
		}
	})

	t.Run("janela estendida já vencida expira normalmente", func(t *testing.T) {
		fx := seedScaleEvent(t)
		productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
		// Cliente promovido não pagou: janela do cart E da promoção já vencidas.
		cartID := seedNotifiedCart(t, fx, productID,
			time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))

		svc.ExpireCart(ctx, cartID, fx.storeID)

		if st := cartStatus(t, cartID); st != "expired" {
			t.Fatalf("cart promovido com janela vencida deveria expirar, got %s", st)
		}
	})
}

// ============================================================================
// (D) Invariante sob concorrência: ExpireCart(waitlister) × promoção do holder
// ============================================================================

func TestExpiryPromotionConcurrentRaceInvariant(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)

	// Muitos setups independentes, cada um dispara a corrida holder×waitlister em
	// paralelo. Rodar com -race para expor qualquer interleaving corrompido.
	const ROUNDS = 80
	for round := 0; round < ROUNDS; round++ {
		productID := seedSoldOutProductWithQueue(t, fx, 0, 1)
		holderCart := seedHolderCart(t, fx, productID, 1)

		var waiterCart string
		if err := testPool.QueryRow(ctx,
			`SELECT cart_id::text FROM waitlist_items WHERE product_id = $1 AND status = 'waiting' LIMIT 1`,
			productID).Scan(&waiterCart); err != nil {
			t.Fatalf("round %d: lookup waiter cart: %v", round, err)
		}
		past := time.Now().Add(-time.Minute)
		setCartExpiresAt(t, holderCart, past)
		setCartExpiresAt(t, waiterCart, past)

		// As DUAS tasks cart.expire (holder e waitlister) disparam concorrentes.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); svc.ExpireCart(ctx, holderCart, fx.storeID) }()
		go func() { defer wg.Done(); svc.ExpireCart(ctx, waiterCart, fx.storeID) }()
		wg.Wait()

		// Invariante forte: NUNCA um cart notified+expired dentro da janela.
		if n := countNotifiedOnExpiredCartsInWindow(t, productID); n != 0 {
			t.Fatalf("round %d: notified+expired dentro da janela (%d) — corrupção da corrida", round, n)
		}
		// Garantia do dono: o próximo da fila é SEMPRE promovido quando o estoque
		// libera, e o cart dele nunca expira pelo próprio timer durante a promoção.
		if st := waitlistStatusOnCart(t, waiterCart); st != "notified" {
			t.Fatalf("round %d: waitlister deveria ser SEMPRE promovido, status=%s", round, st)
		}
		if st := cartStatus(t, waiterCart); st == "expired" {
			t.Fatalf("round %d: cart do waitlister promovido não pode expirar", round)
		}
		// Conservação: a unidade do holder acabou nas mãos do promovido (held vivo
		// =1, estoque=0) — sem vazamento nem duplicação.
		if held := unitsHeldAvailableLive(t, productID); held != 1 {
			t.Fatalf("round %d: held=%d, esperado 1 (unidade com o promovido)", round, held)
		}
		if s := productStock(t, productID); s != 0 {
			t.Fatalf("round %d: estoque=%d, esperado 0", round, s)
		}
	}
}

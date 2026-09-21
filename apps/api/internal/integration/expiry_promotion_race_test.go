package integration

// Expiration and promotion share the cart deadline. Waiting never prevents an
// expired cart from closing; promotion never starts a second item-level timer.

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

func TestExpiryPromotionWaitingCartExpiresAtDeadline(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 1)
	var cartID string
	if err := testPool.QueryRow(ctx,
		`SELECT cart_id::text FROM waitlist_items WHERE product_id = $1 AND status = 'waiting' LIMIT 1`,
		productID).Scan(&cartID); err != nil {
		t.Fatal(err)
	}
	setCartExpiresAt(t, cartID, time.Now().Add(-time.Minute))
	if err := svc.RunScheduledExpiry(ctx, cartID); err != nil {
		t.Fatal(err)
	}
	svc.ExpireCart(ctx, cartID, fx.storeID)
	if st := cartStatus(t, cartID); st != "expired" {
		t.Fatalf("waiting cart failed to expire: %s", st)
	}
	if st := waitlistStatusOnCart(t, cartID); st == "waiting" {
		t.Fatal("expired cart retained an active waiting balance")
	}
	if stock := productStock(t, productID); stock != 0 {
		t.Fatalf("unallocated units were returned to stock: %d", stock)
	}
}

func TestExpiryPromotionHolderExpiryPreservesWaiterDeadline(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 1)
	holderCart := seedHolderCart(t, fx, productID, 1)
	var waiterCart string
	if err := testPool.QueryRow(ctx,
		`SELECT cart_id::text FROM waitlist_items WHERE product_id = $1 AND status = 'waiting' LIMIT 1`,
		productID).Scan(&waiterCart); err != nil {
		t.Fatal(err)
	}
	setCartExpiresAt(t, holderCart, time.Now().Add(-time.Minute))
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	setCartExpiresAt(t, waiterCart, deadline)
	svc.ExpireCart(ctx, holderCart, fx.storeID)
	if st := cartStatus(t, holderCart); st != "expired" {
		t.Fatalf("holder should expire: %s", st)
	}
	if st := waitlistStatusOnCart(t, waiterCart); st != "notified" {
		t.Fatalf("eligible waiter should be promoted: %s", st)
	}
	if q := waitlistedQtyOnCart(t, waiterCart, productID); q != 0 {
		t.Fatalf("pending quantity = %d, want 0", q)
	}
	var actual time.Time
	var itemDeadline *time.Time
	if err := testPool.QueryRow(ctx, `SELECT c.expires_at, wi.expires_at FROM carts c
	    JOIN waitlist_items wi ON wi.cart_id = c.id WHERE c.id = $1`, waiterCart).Scan(&actual, &itemDeadline); err != nil {
		t.Fatal(err)
	}
	if !actual.Equal(deadline) || itemDeadline != nil {
		t.Fatalf("promotion changed deadline or created item timer: cart=%v item=%v want=%v", actual, itemDeadline, deadline)
	}
	if stock := productStock(t, productID); stock != 0 {
		t.Fatalf("stock = %d, want 0", stock)
	}
}

func TestExpiryPromotionOnlyCartDeadlineMatters(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	for _, tc := range []struct {
		name                   string
		cartOffset, itemOffset time.Duration
		expired                bool
	}{
		{"future cart and expired legacy item", time.Hour, -time.Minute, false},
		{"expired cart and future legacy item", -time.Minute, time.Hour, true},
		{"both expired", -time.Minute, -time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedScaleEvent(t)
			productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
			cartID := seedNotifiedCart(t, fx, productID,
				time.Now().Add(tc.cartOffset), time.Now().Add(tc.itemOffset))
			svc.ExpireCart(ctx, cartID, fx.storeID)
			if expired := cartStatus(t, cartID) == "expired"; expired != tc.expired {
				t.Fatalf("expired = %v, want %v", expired, tc.expired)
			}
		})
	}
}

func TestExpiryPromotionConcurrentExpiredWaiterCannotResurrect(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := scaleService()
	fx := seedScaleEvent(t)
	for round := 0; round < 30; round++ {
		productID := seedSoldOutProductWithQueue(t, fx, 0, 1)
		holderCart := seedHolderCart(t, fx, productID, 1)
		var waiterCart string
		if err := testPool.QueryRow(ctx,
			`SELECT cart_id::text FROM waitlist_items WHERE product_id = $1 AND status = 'waiting' LIMIT 1`,
			productID).Scan(&waiterCart); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Minute)
		setCartExpiresAt(t, holderCart, past)
		setCartExpiresAt(t, waiterCart, past)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); svc.ExpireCart(ctx, holderCart, fx.storeID) }()
		go func() { defer wg.Done(); svc.ExpireCart(ctx, waiterCart, fx.storeID) }()
		wg.Wait()
		if st := cartStatus(t, waiterCart); st != "expired" {
			t.Fatalf("round %d: expired waiter resurrected: %s", round, st)
		}
		if st := waitlistStatusOnCart(t, waiterCart); st == "waiting" || st == "notified" {
			t.Fatalf("round %d: expired waiter remained eligible: %s", round, st)
		}
		if held := unitsHeldAvailableLive(t, productID); held != 0 {
			t.Fatalf("round %d: held=%d, want 0", round, held)
		}
		if stock := productStock(t, productID); stock != 1 {
			t.Fatalf("round %d: stock=%d, want 1", round, stock)
		}
	}
}

package order_test

// Testes TDD para a Fatia 2: ciclo de vida DRAFT (pending_payment → paid | expired).
//
// Invariantes cobertas:
//   D1 EnsureOrderForCheckout cria draft (status=pending_payment, totais NULL)
//   D2 EnsureOrderForCheckout é idempotente
//   D3 OnCartPaid sobre draft TRANSICIONA a MESMA linha (count=1, status=paid)
//   D4 OnCartPaid sobre draft preenche totais e itens corretos
//   D5 OnCartPaid sem draft prévio ainda materializa em paid (fallback Fatia 1)
//   D6 OnCartExpired: pending_payment → expired
//   D7 OnCartExpired idempotente (2ª entrega = no-op)
//   D8 OnCartExpired sem draft → no-op (não existe order)

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedCheckoutCart cria uma cart em status 'checkout' (não paga) com 1 produto.
// Retorna os IDs para os testes de draft.
func seedCheckoutCart(t *testing.T, qty int32, unitPrice int64) seedCartResult {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Draft Test Store', 'draft-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seedCheckoutCart store: %v", err)
	}

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'live', 'Ev Draft') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seedCheckoutCart event: %v", err)
	}

	kw := fmt.Sprintf("%d", time.Now().UnixNano()%8000+1000)
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Prod Draft', 'none', 'dext-'||$2, $3, $4, 100) RETURNING id::text`,
		storeID, n, kw, unitPrice,
	).Scan(&productID); err != nil {
		t.Fatalf("seedCheckoutCart product: %v", err)
	}

	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, coupon_discount_cents)
		 VALUES ($1, 'ud-'||$2, '@bd'||$2, 'dtok-'||$2, ($2)::bigint % 100000,
		   'checkout', 'pending', 0) RETURNING id::text`,
		eventID, n,
	).Scan(&cartID); err != nil {
		t.Fatalf("seedCheckoutCart cart: %v", err)
	}

	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, $4, 0)`,
		cartID, productID, qty, unitPrice,
	); err != nil {
		t.Fatalf("seedCheckoutCart cart_items: %v", err)
	}

	return seedCartResult{storeID: storeID, eventID: eventID, cartID: cartID, productID: productID}
}

// markCartPaid atualiza a cart para payment_status='paid' e paid_at=now().
func markCartPaid(t *testing.T, cartID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET payment_status='paid', paid_at=now() WHERE id=$1`, cartID,
	); err != nil {
		t.Fatalf("markCartPaid: %v", err)
	}
}

// ─── D1 EnsureOrderForCheckout cria draft ────────────────────────────────────

func TestEnsureOrderForCheckout_CreatesDraft(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 2, 5000)

	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	var status string
	var totalCents, paidTotalCents *int64
	if err := testPool.QueryRow(ctx,
		`SELECT status, total_cents, paid_total_cents FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&status, &totalCents, &paidTotalCents); err != nil {
		t.Fatalf("query draft order: %v", err)
	}

	if status != "pending_payment" {
		t.Errorf("D1: want status=pending_payment, got %s", status)
	}
	if totalCents != nil {
		t.Errorf("D1: total_cents should be NULL on draft, got %d", *totalCents)
	}
	if paidTotalCents != nil {
		t.Errorf("D1: paid_total_cents should be NULL on draft, got %d", *paidTotalCents)
	}

	// payment and logistics rows created 1:1
	var paymentStatus string
	if err := testPool.QueryRow(ctx,
		`SELECT op.payment_status FROM order_payments op
		 JOIN orders o ON o.id = op.order_id WHERE o.cart_id = $1`, r.cartID,
	).Scan(&paymentStatus); err != nil {
		t.Fatalf("query draft payment: %v", err)
	}
	if paymentStatus != "pending" {
		t.Errorf("D1: want payment_status=pending, got %s", paymentStatus)
	}

	var logCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_logistics ol JOIN orders o ON o.id = ol.order_id WHERE o.cart_id = $1`, r.cartID,
	).Scan(&logCount); err != nil {
		t.Fatalf("query draft logistics: %v", err)
	}
	if logCount != 1 {
		t.Errorf("D1: want 1 logistics row, got %d", logCount)
	}
}

// ─── D2 EnsureOrderForCheckout é idempotente ─────────────────────────────────

func TestEnsureOrderForCheckout_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 3000)

	for i := 0; i < 3; i++ {
		l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)
	}

	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&count); err != nil {
		t.Fatalf("D2 query: %v", err)
	}
	if count != 1 {
		t.Errorf("D2: expected 1 order after 3 calls, got %d", count)
	}
}

// ─── D3 OnCartPaid sobre draft TRANSICIONA A MESMA linha ─────────────────────

func TestOnCartPaid_SealsDraft_SameRow(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 3, 10000)

	// Create draft
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	// Read draft order ID
	var draftOrderID string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&draftOrderID); err != nil {
		t.Fatalf("D3 read draft id: %v", err)
	}

	// Mark cart as paid and seal
	markCartPaid(t, r.cartID)
	gmv := int64(3 * 10000)
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, gmv, nil); err != nil {
		t.Fatalf("D3 OnCartPaid: %v", err)
	}

	// Must still be exactly 1 order row
	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&count); err != nil {
		t.Fatalf("D3 count: %v", err)
	}
	if count != 1 {
		t.Errorf("D3: expected 1 order (same row), got %d", count)
	}

	// The ID must be the SAME as the draft's
	var sealedOrderID string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&sealedOrderID); err != nil {
		t.Fatalf("D3 sealed id: %v", err)
	}
	if sealedOrderID != draftOrderID {
		t.Errorf("D3: order_id changed after seal: draft=%s sealed=%s", draftOrderID, sealedOrderID)
	}

	// Status must be paid
	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&status); err != nil {
		t.Fatalf("D3 status: %v", err)
	}
	if status != "paid" {
		t.Errorf("D3: want status=paid, got %s", status)
	}
}

// ─── D4 OnCartPaid sobre draft preenche totais corretos ──────────────────────

func TestOnCartPaid_SealsDraft_CorrectTotals(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	// 2 items * 15000 = 30000 GMV, 2000 discount, 1500 shipping → paid = 29500
	r := seedCheckoutCart(t, 2, 15000)
	// Set discount and shipping directly on the cart
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET coupon_discount_cents=2000, shipping_cost_cents=1500 WHERE id=$1`, r.cartID,
	); err != nil {
		t.Fatalf("D4 update cart: %v", err)
	}

	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)
	markCartPaid(t, r.cartID)

	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 30000, nil); err != nil {
		t.Fatalf("D4 OnCartPaid: %v", err)
	}

	var total, discount, shipping, paidTotal int64
	if err := testPool.QueryRow(ctx,
		`SELECT total_cents, discount_cents, shipping_cents, paid_total_cents
		 FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&total, &discount, &shipping, &paidTotal); err != nil {
		t.Fatalf("D4 query: %v", err)
	}

	if total != 30000 {
		t.Errorf("D4: total_cents want 30000, got %d", total)
	}
	want := total - discount + shipping
	if paidTotal != want {
		t.Errorf("D4: paid_total_cents want %d (30000-2000+1500), got %d", want, paidTotal)
	}

	// Items must exist after seal
	var itemCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.cart_id = $1`, r.cartID,
	).Scan(&itemCount); err != nil {
		t.Fatalf("D4 item count: %v", err)
	}
	if itemCount != 1 {
		t.Errorf("D4: want 1 order_item, got %d", itemCount)
	}
}

// ─── D5 OnCartPaid sem draft prévio ainda materializa em paid ────────────────
// (Já coberto pelo TestOnCartPaid_P4_Idempotence — mas testamos explicitamente
//  o caso sem EnsureOrderForCheckout para garantir o fallback.)

func TestOnCartPaid_FallbackNoDraft(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 2, 5000, 0, 0)

	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 10000, nil); err != nil {
		t.Fatalf("D5 OnCartPaid: %v", err)
	}

	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&status); err != nil {
		t.Fatalf("D5 query: %v", err)
	}
	if status != "paid" {
		t.Errorf("D5: want status=paid, got %s", status)
	}
}

// ─── D5b OnCartPaid sobre draft é idempotente (2ª entrega = no-op) ───────────

func TestOnCartPaid_SealsDraft_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 8000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)
	markCartPaid(t, r.cartID)

	for i := 0; i < 3; i++ {
		if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 8000, nil); err != nil {
			t.Fatalf("D5b call %d: %v", i+1, err)
		}
	}

	var orderCount, itemCount int
	testPool.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE cart_id = $1`, r.cartID).Scan(&orderCount)
	testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.cart_id = $1`, r.cartID,
	).Scan(&itemCount)

	if orderCount != 1 {
		t.Errorf("D5b: expected 1 order, got %d", orderCount)
	}
	if itemCount != 1 {
		t.Errorf("D5b: expected 1 order_item, got %d", itemCount)
	}
}

// ─── D6 OnCartExpired: pending_payment → expired ─────────────────────────────

func TestOnCartExpired_ExpiresDraft(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 4000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	if err := l.OnCartExpired(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("D6 OnCartExpired: %v", err)
	}

	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM orders WHERE cart_id = $1`, r.cartID,
	).Scan(&status); err != nil {
		t.Fatalf("D6 query: %v", err)
	}
	if status != "expired" {
		t.Errorf("D6: want status=expired, got %s", status)
	}
}

// ─── D7 OnCartExpired idempotente ────────────────────────────────────────────

func TestOnCartExpired_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 6000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	for i := 0; i < 3; i++ {
		if err := l.OnCartExpired(ctx, r.cartID, r.storeID); err != nil {
			t.Fatalf("D7 call %d: %v", i+1, err)
		}
	}

	var count int
	testPool.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE cart_id=$1`, r.cartID).Scan(&count)
	if count != 1 {
		t.Errorf("D7: expected 1 order after 3 expiry calls, got %d", count)
	}

	var status string
	testPool.QueryRow(ctx, `SELECT status FROM orders WHERE cart_id=$1`, r.cartID).Scan(&status)
	if status != "expired" {
		t.Errorf("D7: want status=expired, got %s", status)
	}
}

// ─── D8 OnCartExpired sem draft → no-op ──────────────────────────────────────

func TestOnCartExpired_NoDraft_NoOp(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 2000)
	// Deliberately skip EnsureOrderForCheckout — no Order at all.

	if err := l.OnCartExpired(ctx, r.cartID, r.storeID); err != nil {
		t.Errorf("D8: expected nil error when no order exists, got %v", err)
	}

	var count int
	testPool.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE cart_id=$1`, r.cartID).Scan(&count)
	if count != 0 {
		t.Errorf("D8: expected 0 orders, got %d", count)
	}
}

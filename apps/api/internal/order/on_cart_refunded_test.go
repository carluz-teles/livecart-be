package order_test

// Testes de paridade para os reactors terminais da Order (Fatia E1):
// OnCartRefunded / OnCartCancelled. Gated em TEST_DATABASE_URL (mesma infra do
// on_cart_paid_test.go — reusa seedPaidCart / newListener / requireDB).
//
// Invariantes travadas:
//   R1 cart.refunded  → orders.status='refunded'  E order_payments.payment_status='refunded'
//   R2 cart.cancelled → orders.status='cancelled' E order_payments.payment_status='cancelled'
//   R3 idempotência: 2ª chamada não muta e não retorna erro
//   R4 transição inválida (order inexistente / order não-paga) → erro, sem mutação
//   R5 os sentinels distinguem no-order (ErrOrderNotMaterialised) de estado inválido

import (
	"context"
	"errors"
	"testing"

	"livecart/apps/api/internal/order/listeners"
)

// orderStatuses lê o par (orders.status, order_payments.payment_status) para o cart.
func orderStatuses(t *testing.T, cartID string) (orderStatus, paymentStatus string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT o.status, op.payment_status
		FROM orders o
		JOIN order_payments op ON op.order_id = o.id
		WHERE o.cart_id = $1`, cartID,
	).Scan(&orderStatus, &paymentStatus); err != nil {
		t.Fatalf("orderStatuses(%s): %v", cartID, err)
	}
	return orderStatus, paymentStatus
}

// seedMaterialisedOrder cria um cart pago E materializa a Order (status=paid).
func seedMaterialisedOrder(t *testing.T, l *listeners.Listener) seedCartResult {
	t.Helper()
	r := seedPaidCart(t, 2, 5000, 0, 0) // gmv = 10000
	if err := l.OnCartPaid(context.Background(), r.cartID, r.storeID, 10000, nil); err != nil {
		t.Fatalf("seedMaterialisedOrder OnCartPaid: %v", err)
	}
	return r
}

// ─── R1 refunded flip ────────────────────────────────────────────────────────

func TestOnCartRefunded_R1_FlipsOrderAndPayment(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)

	if err := l.OnCartRefunded(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("OnCartRefunded: %v", err)
	}

	os, ps := orderStatuses(t, r.cartID)
	if os != "refunded" {
		t.Errorf("R1: orders.status=%q, want refunded", os)
	}
	if ps != "refunded" {
		t.Errorf("R1: order_payments.payment_status=%q, want refunded", ps)
	}
}

// ─── R2 cancelled flip ───────────────────────────────────────────────────────

func TestOnCartCancelled_R2_FlipsOrderAndPayment(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)

	if err := l.OnCartCancelled(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("OnCartCancelled: %v", err)
	}

	os, ps := orderStatuses(t, r.cartID)
	if os != "cancelled" {
		t.Errorf("R2: orders.status=%q, want cancelled", os)
	}
	if ps != "cancelled" {
		t.Errorf("R2: order_payments.payment_status=%q, want cancelled", ps)
	}
}

// ─── R3 idempotência: 2ª chamada não muta nem erra ───────────────────────────

func TestOnCartRefunded_R3_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)

	// 1º flip: paid → refunded (muta).
	if err := l.OnCartRefunded(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("OnCartRefunded first call: %v", err)
	}

	var updatedAt1 string
	testPool.QueryRow(ctx, `SELECT updated_at FROM orders WHERE cart_id = $1`, r.cartID).Scan(&updatedAt1)

	// Chamadas repetidas: no-op (status já == target). Não mutam nem erram.
	for i := 0; i < 2; i++ {
		if err := l.OnCartRefunded(ctx, r.cartID, r.storeID); err != nil {
			t.Fatalf("OnCartRefunded idempotent call %d: %v", i+2, err)
		}
	}

	var updatedAt2 string
	testPool.QueryRow(ctx, `SELECT updated_at FROM orders WHERE cart_id = $1`, r.cartID).Scan(&updatedAt2)

	os, ps := orderStatuses(t, r.cartID)
	if os != "refunded" || ps != "refunded" {
		t.Errorf("R3: after repeated calls got (%q,%q), want (refunded,refunded)", os, ps)
	}
	// Prova do no-op: o caminho idempotente não emite UPDATE, então updated_at
	// não muda entre a 2ª e a 3ª chamada.
	if updatedAt1 != updatedAt2 {
		t.Errorf("R3: idempotent calls mutated updated_at (%s → %s); expected no-op", updatedAt1, updatedAt2)
	}
}

func TestOnCartCancelled_R3_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)

	for i := 0; i < 3; i++ {
		if err := l.OnCartCancelled(ctx, r.cartID, r.storeID); err != nil {
			t.Fatalf("OnCartCancelled call %d: %v", i+1, err)
		}
	}

	os, ps := orderStatuses(t, r.cartID)
	if os != "cancelled" || ps != "cancelled" {
		t.Errorf("R3: after 3 calls got (%q,%q), want (cancelled,cancelled)", os, ps)
	}
}

// ─── R4 transição inválida: order inexistente → erro, sem mutação ────────────

func TestOnCartRefunded_R4_NoOrder_Errors(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	// Cart pago mas SEM Order materializada (não chama OnCartPaid).
	r := seedPaidCart(t, 1, 5000, 0, 0)

	err := l.OnCartRefunded(ctx, r.cartID, r.storeID)
	if err == nil {
		t.Fatal("R4: expected error for missing order, got nil")
	}
	if !errors.Is(err, listeners.ErrOrderNotMaterialised) {
		t.Errorf("R4: want ErrOrderNotMaterialised, got %v", err)
	}

	// Nenhuma Order foi criada como efeito colateral.
	var n int
	testPool.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE cart_id = $1`, r.cartID).Scan(&n)
	if n != 0 {
		t.Errorf("R4: expected 0 orders, got %d", n)
	}
}

// ─── R4b transição inválida: order não-paga (já refunded) → erro, sem mutação ─

func TestOnCartCancelled_R4b_NonPaidOrder_Errors(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)

	// Primeiro estorna (paid → refunded).
	if err := l.OnCartRefunded(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("setup OnCartRefunded: %v", err)
	}

	// Agora cancelar uma order já refunded é transição inválida.
	err := l.OnCartCancelled(ctx, r.cartID, r.storeID)
	if err == nil {
		t.Fatal("R4b: expected error cancelling a refunded order, got nil")
	}
	if !errors.Is(err, listeners.ErrInvalidOrderTransition) {
		t.Errorf("R4b: want ErrInvalidOrderTransition, got %v", err)
	}

	// Estado permanece refunded — o cancel inválido não mutou nada.
	os, ps := orderStatuses(t, r.cartID)
	if os != "refunded" || ps != "refunded" {
		t.Errorf("R4b: state mutated to (%q,%q), want it to stay (refunded,refunded)", os, ps)
	}
}

// ─── R5 sentinels distintos ──────────────────────────────────────────────────

func TestTerminalReactors_R5_SentinelsAreDistinct(t *testing.T) {
	if errors.Is(listeners.ErrOrderNotMaterialised, listeners.ErrInvalidOrderTransition) {
		t.Error("R5: sentinels must be distinct errors")
	}
}

package order_test

// Testes TDD para a Fatia 3: espelho ERP → Order (order_logistics + order_payments).
//
// Invariantes cobertas:
//   E1 order_logistics reflete erp_order_state e erp_stock_launched do cart
//   E2 order_payments reflete external_order_id, erp_finalisation_status e invoice
//   E3 idempotência: 2× mirror = mesmo estado
//   E4 no-op quando não há Order para o cart (best-effort)
//   E5 OnCartPaid chama o mirror automaticamente (estado ERP aparece na Order)

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// setCartERPOrderState configura os campos ERP de reserva diretamente no cart.
func setCartERPOrderState(t *testing.T, cartID, state string, stockLaunched bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE carts
		 SET erp_order_state    = $2,
		     erp_stock_launched = $3,
		     erp_op_started_at  = now()
		 WHERE id = $1`,
		cartID, state, stockLaunched,
	); err != nil {
		t.Fatalf("setCartERPOrderState: %v", err)
	}
}

// setCartERPFinalisation configura os campos ERP de finalização no cart.
func setCartERPFinalisation(t *testing.T, cartID, externalOrderID, finalisationStatus string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE carts
		 SET external_order_id        = $2,
		     erp_finalisation_status  = $3,
		     erp_last_error           = 'some-error',
		     erp_last_attempt_at      = now(),
		     erp_attempts_count       = 1
		 WHERE id = $1`,
		cartID, externalOrderID, finalisationStatus,
	); err != nil {
		t.Fatalf("setCartERPFinalisation: %v", err)
	}
}

// setCartERPInvoice configura os campos de NFe no cart.
func setCartERPInvoice(t *testing.T, cartID, invoiceID, invoiceKey, invoiceStatus string) {
	t.Helper()
	ctx := context.Background()
	// chave NFe de 44 dígitos (formato real)
	if len(invoiceKey) != 44 {
		invoiceKey = fmt.Sprintf("%044d", time.Now().UnixNano()%1000000000000)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts
		 SET erp_invoice_id         = $2,
		     erp_invoice_key        = $3,
		     erp_invoice_status     = $4,
		     erp_invoice_emitted_at = now()
		 WHERE id = $1`,
		cartID, invoiceID, invoiceKey, invoiceStatus,
	); err != nil {
		t.Fatalf("setCartERPInvoice: %v", err)
	}
}

// ─── E1 order_logistics reflete erp_order_state e erp_stock_launched ────────

func TestMirrorCartERPToOrder_E1_Logistics(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 5000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	// Cart com reserva ERP criada.
	setCartERPOrderState(t, r.cartID, "open", true)

	l.MirrorCartERPToOrder(ctx, r.cartID)

	var erpState string
	var stockLaunched bool
	var opStartedAt *time.Time
	if err := testPool.QueryRow(ctx, `
		SELECT ol.erp_order_state, ol.erp_stock_launched, ol.erp_op_started_at
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.cart_id = $1
	`, r.cartID).Scan(&erpState, &stockLaunched, &opStartedAt); err != nil {
		t.Fatalf("E1 query logistics: %v", err)
	}

	if erpState != "open" {
		t.Errorf("E1: erp_order_state want 'open', got %q", erpState)
	}
	if !stockLaunched {
		t.Error("E1: erp_stock_launched want true, got false")
	}
	if opStartedAt == nil {
		t.Error("E1: erp_op_started_at should be set, got nil")
	}
}

// ─── E1b order_logistics reflete erp_order_state='cancelled' ─────────────────

func TestMirrorCartERPToOrder_E1b_LogisticsCancelled(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 3000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	setCartERPOrderState(t, r.cartID, "cancelled", false)

	l.MirrorCartERPToOrder(ctx, r.cartID)

	var erpState string
	if err := testPool.QueryRow(ctx, `
		SELECT ol.erp_order_state
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		WHERE o.cart_id = $1
	`, r.cartID).Scan(&erpState); err != nil {
		t.Fatalf("E1b query: %v", err)
	}
	if erpState != "cancelled" {
		t.Errorf("E1b: erp_order_state want 'cancelled', got %q", erpState)
	}
}

// ─── E2 order_payments reflete external_order_id + finalisation + invoice ────

func TestMirrorCartERPToOrder_E2_Payments(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 2, 10000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	setCartERPFinalisation(t, r.cartID, "tiny-ext-789", "done")
	invoiceKey := fmt.Sprintf("%044d", time.Now().UnixNano()%10000000000000000)
	setCartERPInvoice(t, r.cartID, "nf-001", invoiceKey, "authorized")

	l.MirrorCartERPToOrder(ctx, r.cartID)

	var extID, finStatus, invID, invKey, invStatus string
	var attemptsCount int32
	if err := testPool.QueryRow(ctx, `
		SELECT op.external_order_id,
		       op.erp_finalisation_status,
		       op.erp_attempts_count,
		       COALESCE(op.invoice_id, ''),
		       COALESCE(op.invoice_key, ''),
		       COALESCE(op.invoice_status, '')
		FROM order_payments op
		JOIN orders o ON o.id = op.order_id
		WHERE o.cart_id = $1
	`, r.cartID).Scan(&extID, &finStatus, &attemptsCount, &invID, &invKey, &invStatus); err != nil {
		t.Fatalf("E2 query payments: %v", err)
	}

	if extID != "tiny-ext-789" {
		t.Errorf("E2: external_order_id want 'tiny-ext-789', got %q", extID)
	}
	if finStatus != "done" {
		t.Errorf("E2: erp_finalisation_status want 'done', got %q", finStatus)
	}
	if attemptsCount != 1 {
		t.Errorf("E2: erp_attempts_count want 1, got %d", attemptsCount)
	}
	if invID != "nf-001" {
		t.Errorf("E2: invoice_id want 'nf-001', got %q", invID)
	}
	if invKey != invoiceKey {
		t.Errorf("E2: invoice_key want %q, got %q", invoiceKey, invKey)
	}
	if invStatus != "authorized" {
		t.Errorf("E2: invoice_status want 'authorized', got %q", invStatus)
	}
}

// ─── E3 idempotência: 2× mirror = mesmo estado ────────────────────────────────

func TestMirrorCartERPToOrder_E3_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 1, 7000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	setCartERPOrderState(t, r.cartID, "open", true)
	setCartERPFinalisation(t, r.cartID, "ext-idem", "done")

	// Chamar 3 vezes — resultado deve ser o mesmo.
	for i := 0; i < 3; i++ {
		l.MirrorCartERPToOrder(ctx, r.cartID)
	}

	var erpState string
	var extID string
	if err := testPool.QueryRow(ctx, `
		SELECT ol.erp_order_state, COALESCE(op.external_order_id, '')
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		JOIN order_payments op ON op.order_id = o.id
		WHERE o.cart_id = $1
	`, r.cartID).Scan(&erpState, &extID); err != nil {
		t.Fatalf("E3 query: %v", err)
	}

	if erpState != "open" {
		t.Errorf("E3: erp_order_state want 'open', got %q", erpState)
	}
	if extID != "ext-idem" {
		t.Errorf("E3: external_order_id want 'ext-idem', got %q", extID)
	}
}

// ─── E4 no-op quando não há Order para o cart ─────────────────────────────────

func TestMirrorCartERPToOrder_E4_NoOp_NoOrder(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	// Cart sem Order associada.
	r := seedCheckoutCart(t, 1, 4000)
	setCartERPOrderState(t, r.cartID, "open", true)

	// Deve executar sem erro (best-effort).
	l.MirrorCartERPToOrder(ctx, r.cartID)

	var count int
	testPool.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE cart_id = $1`, r.cartID).Scan(&count)
	if count != 0 {
		t.Errorf("E4: mirror should not create orders, got %d", count)
	}
}

// ─── E5 OnCartPaid chama o mirror automaticamente ────────────────────────────

func TestOnCartPaid_E5_MirrorsERPState(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedCheckoutCart(t, 2, 8000)
	l.EnsureOrderForCheckout(ctx, r.cartID, r.storeID)

	// Seta estado ERP ANTES de pagar (simula checkout que iniciou reserva).
	setCartERPOrderState(t, r.cartID, "open", true)
	setCartERPFinalisation(t, r.cartID, "tiny-confirm-42", "done")

	markCartPaid(t, r.cartID)
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 16000, nil); err != nil {
		t.Fatalf("E5 OnCartPaid: %v", err)
	}

	var erpState string
	var extID string
	if err := testPool.QueryRow(ctx, `
		SELECT ol.erp_order_state, COALESCE(op.external_order_id, '')
		FROM order_logistics ol
		JOIN orders o ON o.id = ol.order_id
		JOIN order_payments op ON op.order_id = o.id
		WHERE o.cart_id = $1
	`, r.cartID).Scan(&erpState, &extID); err != nil {
		t.Fatalf("E5 query: %v", err)
	}

	if erpState != "open" {
		t.Errorf("E5: erp_order_state want 'open', got %q", erpState)
	}
	if extID != "tiny-confirm-42" {
		t.Errorf("E5: external_order_id want 'tiny-confirm-42', got %q", extID)
	}
}

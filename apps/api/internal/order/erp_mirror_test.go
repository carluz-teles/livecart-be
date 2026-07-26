package order_test

// Testes TDD para a Fatia 3: espelho ERP → Order (order_logistics + order_payments).
//
// Invariantes cobertas:
//   E1 order_logistics reflete erp_order_state e erp_stock_launched do cart
//   E2 order_payments reflete SÓ o external_order_id (reserva); finalização/NF
//      NÃO são mais projetadas pelo mirror (Fatia 11b: escrita autoritativa
//      direta pelos reactors em order_payments — o mirror clobbaria a fonte)
//   E3 idempotência: 2× mirror = mesmo estado
//   E4 no-op quando não há Order para o cart (best-effort)
//   E5 OnCartPaid chama o mirror automaticamente (estado ERP aparece na Order)

import (
	"context"
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

// setCartExternalOrderID configura o external_order_id (reserva) no cart — o
// único campo pós-reserva que o mirror ainda projeta para order_payments. As
// colunas pós-venda de finalização/NF saíram do cart na Fatia 10-b (vivem só em
// order_payments, escritas autoritativamente pelos reactors), então não há mais
// estado de finalização/NF a semear no cart.
func setCartExternalOrderID(t *testing.T, cartID, externalOrderID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET external_order_id = $2 WHERE id = $1`,
		cartID, externalOrderID,
	); err != nil {
		t.Fatalf("setCartExternalOrderID: %v", err)
	}
}

// ─── E1 order_logistics reflete erp_order_state e erp_stock_launched ────────

func TestMirrorCartERPToOrder_Logistics(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 1, 5000, 0, 0)
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 5000, nil); err != nil {
		t.Fatalf("E1 OnCartPaid: %v", err)
	}
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

func TestMirrorCartERPToOrder_LogisticsCancelled(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 1, 3000, 0, 0)
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 3000, nil); err != nil {
		t.Fatalf("E1b OnCartPaid: %v", err)
	}
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

// ─── E2 order_payments reflete SÓ o external_order_id (reserva) ──────────────
//
// Fatia 11b: a finalização (erp_finalisation_*) e a NF (invoice_*) passaram a
// ser escritas AUTORITATIVAMENTE em order_payments pelos reactors de ERP. O
// mirror não pode projetá-las do cart — se o fizesse, clobbaria a linha
// autoritativa com o valor (talvez defasado) do cart no momento da
// materialização. Fatia 10-b: essas colunas foram inclusive DROPADAS do cart, o
// que torna a garantia estrutural. Este teste trava o mirror: ele projeta APENAS
// o external_order_id (reserva), e as colunas de finalização/NF em order_payments
// permanecem no default ('pending', 0, NULL) — só os reactors as escrevem.
func TestMirrorCartERPToOrder_Payments(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 2, 10000, 0, 0)
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 20000, nil); err != nil {
		t.Fatalf("E2 OnCartPaid: %v", err)
	}
	setCartExternalOrderID(t, r.cartID, "tiny-ext-789")

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

	// external_order_id (reserva) CONTINUA sendo projetado pelo mirror.
	if extID != "tiny-ext-789" {
		t.Errorf("E2: external_order_id want 'tiny-ext-789', got %q", extID)
	}
	// Finalização/NF NÃO são mais projetadas — permanecem no default de
	// order_payments (só os reactors as escrevem, autoritativamente).
	if finStatus != "pending" {
		t.Errorf("E2: mirror não deve projetar finalização — erp_finalisation_status want 'pending' (default), got %q", finStatus)
	}
	if attemptsCount != 0 {
		t.Errorf("E2: mirror não deve projetar finalização — erp_attempts_count want 0 (default), got %d", attemptsCount)
	}
	if invID != "" || invKey != "" || invStatus != "" {
		t.Errorf("E2: mirror não deve projetar NF — invoice_* want vazios (default), got id=%q key=%q status=%q", invID, invKey, invStatus)
	}
}

// ─── E3 idempotência: 2× mirror = mesmo estado ────────────────────────────────

func TestMirrorCartERPToOrder_Idempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 1, 7000, 0, 0)
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 7000, nil); err != nil {
		t.Fatalf("E3 OnCartPaid: %v", err)
	}
	setCartERPOrderState(t, r.cartID, "open", true)
	setCartExternalOrderID(t, r.cartID, "ext-idem")

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

func TestMirrorCartERPToOrder_NoOp_NoOrder(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	// Cart pago mas sem Order materializada.
	r := seedPaidCart(t, 1, 4000, 0, 0)
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

func TestOnCartPaid_MirrorsERPState(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 2, 8000, 0, 0)

	// Seta estado ERP ANTES de materializar a Order (simula reserva já existente).
	setCartERPOrderState(t, r.cartID, "open", true)
	setCartExternalOrderID(t, r.cartID, "tiny-confirm-42")

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

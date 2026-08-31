package order_test

// Fatia B1 — cutover da LEITURA do detalhe do pedido para as tabelas order_*.
//
// Invariantes travadas (gated em TEST_DATABASE_URL):
//   B1a PARIDADE: GetByID retorna, para uma order materializada, exatamente os
//       valores de customer/shipping/erp/itens que estavam no cart no momento
//       do cart.paid (contrato/DTO inalterado — só muda a FONTE).
//   B1b FONTE: após materializar, mutar o cart NÃO altera o detalhe — GetByID lê
//       de orders.customer_snapshot / order_logistics / order_payments /
//       order_items, nunca mais de carts.* (prova o cutover).
//   B1c ESCRITA: o PATCH admin (UpdateStatus/UpdatePaymentStatus) usa o
//       vocabulário de CART (active/checkout/completed/expired; pending/paid/
//       failed/refunded) e escreve SÓ em carts.* — nunca em orders.status /
//       order_payments.payment_status, cujo dono é a materialização + os
//       reactors da slice E1. UpdateShippingAddress escreve order_logistics.

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/order"
)

// seedFrozenOrder seeds a paid cart with rich customer/shipping/ERP data, then
// materialises the Order (freezing that data). Returns the ids so tests can
// mutate the source cart and assert the detail stays frozen.
//
// serviceID is passed through verbatim so callers can exercise both ME-style
// numeric ids and opaque (ObjectId/UUID) ids from other providers.
func seedFrozenOrder(t *testing.T, serviceID string) (storeID, cartID string) {
	t.Helper()
	ctx := context.Background()

	r := seedPaidCart(t, 2, 5000, 0, 1500) // gmv=10000, shipping=1500
	storeID, cartID = r.storeID, r.cartID

	// Rich customer + shipping on the source cart, BEFORE cart.paid. As colunas
	// pós-venda do ERP (erp_finalisation_status/erp_invoice_*) foram DROPADAS do
	// cart na Fatia 10-b — a finalização/NF vivem só em order_payments; ver o
	// write autoritativo abaixo.
	if _, err := testPool.Exec(ctx, `
		UPDATE carts SET
			customer_name          = 'Alice Frozen',
			customer_email         = 'alice@frozen.com',
			customer_document      = '12345678900',
			customer_phone         = '+5511999998888',
			shipping_provider      = 'melhor_envio',
			shipping_service_id    = $2,
			shipping_service_name  = 'SEDEX',
			shipping_carrier       = 'Correios',
			shipping_cost_cents    = 1500,
			shipping_cost_real_cents = 1200,
			shipping_deadline_days = 5,
			shipping_address       = $3
		WHERE id = $1`,
		cartID, serviceID,
		`{"zipCode":"01000-000","street":"Rua A","number":"10","complement":"apto 1","neighborhood":"Centro","city":"São Paulo","state":"SP"}`,
	); err != nil {
		t.Fatalf("seedFrozenOrder update cart: %v", err)
	}

	l := newListener(t)
	if err := l.OnCartPaid(ctx, cartID, storeID, 10000, nil); err != nil {
		t.Fatalf("seedFrozenOrder OnCartPaid: %v", err)
	}

	// Fatia 11b: a finalização/NF do ERP são autoritativas em order_payments,
	// escritas pelos reactors. Simula o reactor gravando o estado terminal direto
	// na fonte autoritativa, para o detalhe (que lê de order_payments) refletir.
	if _, err := testPool.Exec(ctx, `
		UPDATE order_payments op SET
			erp_finalisation_status = 'done',
			invoice_id              = 'INV-1',
			invoice_key             = 'KEY-1',
			invoice_status          = 'authorized'
		FROM orders o
		WHERE o.id = op.order_id AND o.cart_id = $1`, cartID,
	); err != nil {
		t.Fatalf("seedFrozenOrder order_payments ERP: %v", err)
	}
	return storeID, cartID
}

// ─── B1a PARIDADE ─────────────────────────────────────────────────────────────

func TestOrderDetailReadCutover_GetByID_ParityWithSourceCart(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	// Cobre os dois formatos de service_id: ME numérico e opaco (não-ME).
	cases := []struct {
		name      string
		serviceID string
	}{
		{name: "me numeric service id", serviceID: "3"},
		{name: "opaque non-me service id", serviceID: "svc_64f0ab12ObjectId"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cartID := seedFrozenOrder(t, tc.serviceID)

			row, err := repo.GetByID(ctx, cartID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if row == nil {
				t.Fatal("GetByID returned nil for materialised order")
			}

			// Customer (agora de orders.customer_snapshot).
			assertEq(t, "customer_name", row.CustomerName, "Alice Frozen")
			assertEq(t, "customer_email", row.CustomerEmail, "alice@frozen.com")
			assertEq(t, "customer_document", row.CustomerDocument, "12345678900")
			assertEq(t, "customer_phone", row.CustomerPhone, "+5511999998888")

			// Shipping (agora de order_logistics) — service_id opaco preservado.
			assertEq(t, "shipping_provider", row.ShippingProvider, "melhor_envio")
			assertEq(t, "shipping_service_id", row.ShippingServiceID, tc.serviceID)
			assertEq(t, "shipping_service_name", row.ShippingServiceName, "SEDEX")
			assertEq(t, "shipping_carrier", row.ShippingCarrier, "Correios")
			assertEqInt(t, "shipping_cost_cents", int(row.ShippingCostCents), 1500)
			assertEqInt(t, "shipping_cost_real_cents", int(row.ShippingCostRealCents), 1200)
			assertEqInt(t, "shipping_deadline_days", row.ShippingDeadlineDays, 5)

			// shipping_address JSONB decodificado.
			assertEq(t, "ship_addr_zip", row.ShippingAddressZip, "01000-000")
			assertEq(t, "ship_addr_street", row.ShippingAddressStreet, "Rua A")
			assertEq(t, "ship_addr_city", row.ShippingAddressCity, "São Paulo")

			// ERP (agora de order_payments) — mirror copiou do cart.
			assertEq(t, "erp_finalisation_status", row.ERPFinalisationStatus, "done")
			assertEq(t, "erp_invoice_id", row.ERPInvoiceID, "INV-1")
			assertEq(t, "erp_invoice_key", row.ERPInvoiceKey, "KEY-1")
			assertEq(t, "erp_invoice_status", row.ERPInvoiceStatus, "authorized")

			// Status/pagamento congelados = 'paid' na materialização.
			assertEq(t, "status", row.Status, "paid")
			assertEq(t, "payment_status", row.PaymentStatus, "paid")
		})
	}
}

// ─── B1b FONTE (cutover) ──────────────────────────────────────────────────────

func TestOrderDetailReadCutover_GetByID_FrozenAgainstCartMutation(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	_, cartID := seedFrozenOrder(t, "3")

	// Mutar a fonte carts DEPOIS de materializar: o detalhe deve ignorar. (As
	// colunas pós-venda do ERP saíram do cart na Fatia 10-b — a finalização/NF só
	// vivem em order_payments, garantia ainda mais forte de que o detalhe é
	// autoritativo nelas; as asserts erp_* abaixo continuam lendo de order_payments.)
	if _, err := testPool.Exec(ctx, `
		UPDATE carts SET
			customer_name           = 'MUTATED NAME',
			customer_email          = 'mutated@x.com',
			customer_document       = 'MUTATEDDOC',
			customer_phone          = 'MUTATEDPHONE',
			shipping_provider       = 'MUTATED_PROVIDER',
			shipping_service_name   = 'MUTATED_SERVICE',
			shipping_carrier        = 'MUTATED_CARRIER',
			shipping_cost_cents     = 9999,
			shipping_address        = '{"zipCode":"99999","street":"MUTATED"}'
		WHERE id = $1`, cartID,
	); err != nil {
		t.Fatalf("mutate cart: %v", err)
	}

	row, err := repo.GetByID(ctx, cartID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if row == nil {
		t.Fatal("GetByID returned nil")
	}

	// Todos os campos permanecem FROZEN (lidos das tabelas order_*).
	assertEq(t, "customer_name", row.CustomerName, "Alice Frozen")
	assertEq(t, "customer_email", row.CustomerEmail, "alice@frozen.com")
	assertEq(t, "customer_document", row.CustomerDocument, "12345678900")
	assertEq(t, "customer_phone", row.CustomerPhone, "+5511999998888")
	assertEq(t, "shipping_provider", row.ShippingProvider, "melhor_envio")
	assertEq(t, "shipping_service_name", row.ShippingServiceName, "SEDEX")
	assertEq(t, "shipping_carrier", row.ShippingCarrier, "Correios")
	assertEqInt(t, "shipping_cost_cents", int(row.ShippingCostCents), 1500)
	assertEq(t, "ship_addr_street", row.ShippingAddressStreet, "Rua A")
	assertEq(t, "erp_finalisation_status", row.ERPFinalisationStatus, "done")
	assertEq(t, "erp_invoice_id", row.ERPInvoiceID, "INV-1")
}

func TestOrderDetailReadCutover_GetItems_FrozenProductNameFromOrderItems(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	_, cartID := seedFrozenOrder(t, "3")

	// Capturar o nome congelado direto de order_items.
	var frozenName string
	if err := testPool.QueryRow(ctx, `
		SELECT oi.product_name FROM order_items oi
		JOIN orders o ON o.id = oi.order_id
		WHERE o.cart_id = $1 LIMIT 1`, cartID,
	).Scan(&frozenName); err != nil {
		t.Fatalf("read frozen name: %v", err)
	}

	// Renomear o produto vivo — o detalhe deve manter o snapshot da order.
	if _, err := testPool.Exec(ctx,
		`UPDATE products SET name = 'RENAMED LIVE' WHERE id IN (
			SELECT product_id FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.cart_id = $1
		)`, cartID,
	); err != nil {
		t.Fatalf("rename product: %v", err)
	}

	items, err := repo.GetItems(ctx, cartID)
	if err != nil {
		t.Fatalf("GetItems: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("GetItems returned no items for materialised order")
	}
	for _, it := range items {
		if it.ProductName == "RENAMED LIVE" {
			t.Errorf("GetItems leaked live product name %q — deve usar order_items (frozen %q)", it.ProductName, frozenName)
		}
	}
}

// ─── B1c ESCRITA ──────────────────────────────────────────────────────────────

// REGRESSÃO (finding B1-1): o PATCH admin usa o vocabulário de CART e NÃO pode
// corromper o agregado Order. UpdateStatus/UpdatePaymentStatus escrevem SÓ em
// carts.* — orders.status / order_payments.payment_status permanecem no valor da
// materialização (`paid`), pois seu dono é a materialização + os reactors da E1.
func TestOrderDetailReadCutover_AdminUpdate_DoesNotCorruptOrderStatus(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)
	svc := order.NewService(repo, zap.NewNop())

	// Vocabulário real de UpdateOrderRequest (types.go): status de CART
	// (active/checkout/completed/expired) e payment (pending/paid/failed/refunded).
	// 'refunded' SAIU desta tabela, e a saída é a correção.
	//
	// Ele deixou de ser escrita de campo: estorno é FATO. O botão "Marcar como
	// reembolsado" escrevia a coluna e parava aí, e o pedido ficava preso em
	// "Precisam atenção" — fora de "Cancelados" — enquanto a Order seguia paga,
	// o pedido no ERP seguia segurando peça e o cupom seguia consumido.
	//
	// O caso dele agora é TestEstornoSemCasoDeUsoRecusa, logo abaixo.
	cases := []struct {
		name          string
		status        string
		paymentStatus string
	}{
		{"expired + failed", "expired", "failed"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			storeID, cartID := seedFrozenOrder(t, "3")

			// Baseline: o Order nasceu imutável em `paid` na materialização.
			assertDBString(t, ctx, `SELECT status FROM orders WHERE cart_id = $1`, cartID, "paid")
			assertDBString(t, ctx,
				`SELECT op.payment_status FROM order_payments op JOIN orders o ON o.id = op.order_id WHERE o.cart_id = $1`,
				cartID, "paid")

			status, paymentStatus := tt.status, tt.paymentStatus
			if _, err := svc.Update(ctx, order.UpdateOrderInput{
				ID:            cartID,
				StoreID:       storeID,
				Status:        &status,
				PaymentStatus: &paymentStatus,
			}); err != nil {
				t.Fatalf("Service.Update: %v", err)
			}

			// carts.* recebeu a escrita do PATCH admin.
			assertDBString(t, ctx, `SELECT status FROM carts WHERE id = $1`, cartID, tt.status)
			assertDBString(t, ctx, `SELECT payment_status FROM carts WHERE id = $1`, cartID, tt.paymentStatus)

			// ...e o agregado Order permanece INTOCADO (paid) — não corrompido.
			assertDBString(t, ctx, `SELECT status FROM orders WHERE cart_id = $1`, cartID, "paid")
			assertDBString(t, ctx,
				`SELECT op.payment_status FROM order_payments op JOIN orders o ON o.id = op.order_id WHERE o.cart_id = $1`,
				cartID, "paid")
		})
	}
}

// UpdateShippingAddress permanece escrevendo no lado Order (order_logistics) —
// parte do read cutover B1 já aprovado, fora do escopo do fix B1-1.
func TestOrderDetailReadCutover_UpdateShippingAddress_WritesOrderLogistics(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	_, cartID := seedFrozenOrder(t, "3")

	newAddr := map[string]string{"zipCode": "22222-000", "street": "Nova Rua", "city": "Rio"}
	if err := repo.UpdateShippingAddress(ctx, cartID, newAddr); err != nil {
		t.Fatalf("UpdateShippingAddress: %v", err)
	}

	// Read-after-write: o detalhe reflete a escrita em order_logistics.
	row, err := repo.GetByID(ctx, cartID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	assertEq(t, "shipping_address street after update", row.ShippingAddressStreet, "Nova Rua")
	assertEq(t, "shipping_address city after update", row.ShippingAddressCity, "Rio")
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func assertEq(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func assertEqInt(t *testing.T, field string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", field, got, want)
	}
}

func assertDBString(t *testing.T, ctx context.Context, query, arg, want string) {
	t.Helper()
	var got string
	if err := testPool.QueryRow(ctx, query, arg).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if got != want {
		t.Errorf("query %q = %q, want %q", query, got, want)
	}
}

// ESTORNO SEM O CASO DE USO É RECUSA, e não meia escrita.
//
// Um Service sem o colaborador de pagamento não consegue emitir cart.refunded.
// Nesse caso ele TEM de recusar: gravar a coluna assim mesmo reproduziria
// exatamente o defeito que o lojista relatou — pedido estornado preso em
// "Precisam atenção", com a Order paga e o pedido vivo no ERP.
func TestEstornoSemCasoDeUsoRecusa(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)
	svc := order.NewService(repo, zap.NewNop()) // sem SetManualPaymentConfirmer

	storeID, cartID := seedFrozenOrder(t, "9")
	refunded := "refunded"

	if _, err := svc.Update(ctx, order.UpdateOrderInput{
		ID: cartID, StoreID: storeID, PaymentStatus: &refunded,
	}); err == nil {
		t.Fatal("aceitou o estorno sem poder emitir o fato — o pedido ficaria pela metade")
	}

	// E a coluna NÃO foi escrita: meia verdade é pior que recusa.
	assertDBString(t, ctx, `SELECT payment_status FROM carts WHERE id = $1`, cartID, "paid")
}

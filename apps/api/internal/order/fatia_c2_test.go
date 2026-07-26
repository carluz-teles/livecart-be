package order_test

// Fatia C2 — corrige o acoplamento camuflado da tabela `shipments`.
//
// Antes: `shipments.order_id` se chamava "order" mas era FK para carts(id) e
// guardava o UUID do CART; GetShipmentForOrder filtrava por essa coluna tratando
// o cart id como se fosse order id. Depois: existe a coluna `orders_order_id`
// com FK correta para orders(id), e GetShipmentForOrder passa a filtrar por ela.
//
// Invariantes travadas (gated em TEST_DATABASE_URL):
//   C2a FK CERTA: GetShipmentForOrder(orders.id) retorna o shipment; buscar com
//       o cart id NÃO retorna (prova que a coluna certa é orders_order_id).
//   C2b RESOLVE: GetOrderIDByCartID resolve carts.id → orders.id e "" quando não
//       há Order materializada.

import (
	"context"
	"testing"

	"livecart/apps/api/internal/order"
)

// seedShipment insere um shipment ligado ao Order via a FK correta
// (orders_order_id). Mantém order_id = cart id (coluna legada, migration 000052)
// para espelhar o que o INSERT real de produção faz. Retorna (orderID, shipmentID).
func seedShipment(t *testing.T, storeID, cartID string) (orderID, shipmentID string) {
	t.Helper()
	ctx := context.Background()

	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM orders WHERE cart_id = $1`, cartID,
	).Scan(&orderID); err != nil {
		t.Fatalf("seedShipment resolve order id: %v", err)
	}

	if err := testPool.QueryRow(ctx, `
		INSERT INTO shipments (order_id, orders_order_id, store_id, provider, provider_order_id, status)
		VALUES ($1, $2, $3, 'melhor_envio', gen_random_uuid()::text, 'pending')
		RETURNING id::text`,
		cartID, orderID, storeID,
	).Scan(&shipmentID); err != nil {
		t.Fatalf("seedShipment insert: %v", err)
	}
	return orderID, shipmentID
}

// ─── C2a FK CERTA ─────────────────────────────────────────────────────────────

func TestFatiaC2_GetShipmentForOrder_KeyedByOrderID(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	storeID, cartID := seedFrozenOrder(t, "3")
	orderID, shipmentID := seedShipment(t, storeID, cartID)

	// Positivo: buscar com o UUID de orders.id retorna o shipment certo.
	got, err := repo.GetShipmentForOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetShipmentForOrder(orderID): %v", err)
	}
	if got == nil {
		t.Fatal("GetShipmentForOrder(orderID) retornou nil — deveria achar o shipment via orders_order_id")
	}
	if got.ID != shipmentID {
		t.Errorf("shipment id = %q, want %q", got.ID, shipmentID)
	}

	// Negativo (prova do fix): buscar com o UUID do CART NÃO retorna mais nada —
	// antes retornava, porque a coluna order_id guarda o cart id.
	if cartID == orderID {
		t.Fatal("pré-condição inválida: cart id == order id (não prova nada)")
	}
	stale, err := repo.GetShipmentForOrder(ctx, cartID)
	if err != nil {
		t.Fatalf("GetShipmentForOrder(cartID): %v", err)
	}
	if stale != nil {
		t.Errorf("GetShipmentForOrder(cartID) retornou shipment %q — deveria ser nil (não pode mais tratar cart id como order id)", stale.ID)
	}
}

// ─── C2b RESOLVE ──────────────────────────────────────────────────────────────

func TestFatiaC2_GetOrderIDByCartID_Resolves(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	_, cartID := seedFrozenOrder(t, "3")

	// Resolve para o orders.id real (o mesmo que a tabela orders guarda).
	var want string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM orders WHERE cart_id = $1`, cartID,
	).Scan(&want); err != nil {
		t.Fatalf("read orders.id: %v", err)
	}

	got, err := repo.GetOrderIDByCartID(ctx, cartID)
	if err != nil {
		t.Fatalf("GetOrderIDByCartID: %v", err)
	}
	if got != want {
		t.Errorf("GetOrderIDByCartID = %q, want %q", got, want)
	}

	// Cart sem Order materializada → "" sem erro.
	bare := seedPaidCart(t, 1, 1000, 0, 0) // não chama OnCartPaid → sem orders row
	empty, err := repo.GetOrderIDByCartID(ctx, bare.cartID)
	if err != nil {
		t.Fatalf("GetOrderIDByCartID(bare): %v", err)
	}
	if empty != "" {
		t.Errorf("GetOrderIDByCartID(bare) = %q, want \"\" (cart sem Order)", empty)
	}
}

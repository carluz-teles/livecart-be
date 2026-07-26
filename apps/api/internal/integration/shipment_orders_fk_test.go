package integration

// Fatia C2 — CreateShipment popula a FK correta `shipments.orders_order_id`.
//
// O handler de shipping passa `req.ExternalOrderID` (que é o CART id) como
// OrderID. Antes, esse cart id ia parar na coluna `order_id` (FK camuflada para
// carts). Agora CreateShipment resolve o order id real (orders.id) via
// GetOrderIDByCartID e o grava em `orders_order_id`, mantendo `order_id` = cart
// id (coluna legada) para não quebrar os hooks de postcheckout.
//
// Gated em TEST_DATABASE_URL.

import (
	"context"
	"testing"
)

// materialiseOrder cria uma linha mínima em `orders` para o cart, simulando a
// materialização do cart.paid (Fatia A). Retorna o orders.id.
func materialiseOrder(t *testing.T, fx finFixture) string {
	t.Helper()
	var orderID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO orders (cart_id, short_id, store_id, event_id, status)
		VALUES ($1, 1, $2, $3, 'paid')
		RETURNING id::text`,
		fx.cartID, fx.storeID, fx.eventID,
	).Scan(&orderID); err != nil {
		t.Fatalf("materialiseOrder: %v", err)
	}
	return orderID
}

func TestFatiaC2_CreateShipment_PopulatesOrdersOrderID(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedPaidCart(t, 1, 0)
	orderID := materialiseOrder(t, fx)

	// Como o handler: OrderID recebe o CART id (req.ExternalOrderID).
	sh, err := testRepo.CreateShipment(ctx, CreateShipmentParams{
		OrderID:         fx.cartID,
		StoreID:         fx.storeID,
		Provider:        "melhor_envio",
		ProviderOrderID: "prov-c2-create",
		Status:          "pending",
	})
	if err != nil {
		t.Fatalf("CreateShipment: %v", err)
	}

	// order_id (legado) permanece = cart id, para os hooks de postcheckout.
	if sh.OrderID != fx.cartID {
		t.Errorf("shipment.order_id (legado) = %q, want cart id %q", sh.OrderID, fx.cartID)
	}

	// orders_order_id foi populado com o orders.id real (não o cart id).
	var gotOrdersOrderID string
	if err := testPool.QueryRow(ctx,
		`SELECT COALESCE(orders_order_id::text, '') FROM shipments WHERE id = $1`, sh.ID,
	).Scan(&gotOrdersOrderID); err != nil {
		t.Fatalf("read orders_order_id: %v", err)
	}
	if gotOrdersOrderID != orderID {
		t.Errorf("shipment.orders_order_id = %q, want orders.id %q (resolvido do cart id)", gotOrdersOrderID, orderID)
	}
	if gotOrdersOrderID == fx.cartID {
		t.Errorf("orders_order_id foi setado com o CART id %q — deveria ser o orders.id", fx.cartID)
	}
}

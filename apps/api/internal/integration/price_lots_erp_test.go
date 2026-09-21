//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	orderlisteners "livecart/apps/api/internal/order/listeners"
)

func addPriceLot(t *testing.T, cartID, productID string, quantity, waiting int32, price int64, sessionID string) {
	t.Helper()
	cart, _ := parseUUID(cartID)
	product, _ := parseUUID(productID)
	var session pgtype.UUID
	if sessionID != "" {
		session, _ = parseUUID(sessionID)
	}
	_, err := sqlc.New(testPool).UpsertCartItem(context.Background(), sqlc.UpsertCartItemParams{
		CartID: cart, ProductID: product, Quantity: pgtype.Int4{Int32: quantity, Valid: true},
		WaitlistedQuantity: waiting, UnitPrice: pgtype.Int8{Int64: price, Valid: true}, SessionID: session,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPriceLotsERPGridAndOrderSnapshotPreserveExactCents(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	var sessionID string
	if err := testPool.QueryRow(ctx, `INSERT INTO live_sessions(event_id,status,type,sequence_order)
	    VALUES($1,'active','live',1) RETURNING id::text`, fx.eventID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	addPriceLot(t, cartID, productID, 2, 0, 1001, sessionID)
	addPriceLot(t, cartID, productID, 3, 3, 900, sessionID)
	grid, err := testRepo.ListCartGridItems(ctx, cartID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grid) != 2 {
		t.Fatalf("ERP price lines = %+v", grid)
	}
	var total int64
	for _, line := range grid {
		total += int64(line.Quantity) * line.UnitPrice
	}
	if total != 3002 {
		t.Fatalf("ERP total = %d, want 3002", total)
	}
	payload, err := testRepo.ListNonWaitlistedCartItems(ctx, cartID)
	if err != nil {
		t.Fatal(err)
	}
	var payloadTotal int64
	for _, line := range payload {
		payloadTotal += int64(line.Quantity) * line.UnitPrice
	}
	if payloadTotal != total {
		t.Fatalf("payload total = %d, grid = %d", payloadTotal, total)
	}

	// Payment closes the waiting part before the listener seals allocated lots.
	if _, err := testPool.Exec(ctx, `UPDATE carts SET payment_status='paid',paid_at=now() WHERE id=$1`, cartID); err != nil {
		t.Fatal(err)
	}
	listener := orderlisteners.New(testPool, sqlc.New(testPool), zap.NewNop())
	if err := listener.OnCartPaid(ctx, cartID, fx.storeID, 3002, nil); err != nil {
		t.Fatal(err)
	}
	var amount int64
	var quantity, lines, attributed int
	if err := testPool.QueryRow(ctx, `SELECT sum(oi.quantity*oi.unit_price),sum(oi.quantity),count(*),
	    sum(CASE WHEN oi.session_id=$2::uuid THEN oi.quantity ELSE 0 END)
	    FROM order_items oi JOIN orders o ON o.id=oi.order_id WHERE o.cart_id=$1`, cartID, sessionID).
		Scan(&amount, &quantity, &lines, &attributed); err != nil {
		t.Fatal(err)
	}
	if amount != 3002 || quantity != 3 || lines != 2 || attributed != 2 {
		t.Fatalf("order snapshot amount=%d quantity=%d lines=%d attributed=%d", amount, quantity, lines, attributed)
	}
}

func TestPriceLotsERPReflectionPreservesPendingPrices(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := tinyCheckoutProductionRepository(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	addPriceLot(t, cartID, productID, 2, 0, 1001, "")
	addPriceLot(t, cartID, productID, 3, 3, 900, "")
	for range 2 {
		if err := repo.SetCartItemPriceLines(ctx, cartID, productID, []providers.ERPOrderItem{
			{Quantity: 2, UnitPrice: 1000}, {Quantity: 1, UnitPrice: 1101},
		}); err != nil {
			t.Fatal(err)
		}
	}
	var qty, waiting int
	var reserved, pending int64
	if err := testPool.QueryRow(ctx, `SELECT sum(l.quantity),sum(l.waitlisted_quantity),
	    sum((l.quantity-l.waitlisted_quantity)*l.unit_price),sum(l.waitlisted_quantity*l.unit_price)
	    FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id WHERE ci.cart_id=$1`, cartID).
		Scan(&qty, &waiting, &reserved, &pending); err != nil {
		t.Fatal(err)
	}
	if qty != 6 || waiting != 3 || reserved != 3101 || pending != 2700 {
		t.Fatalf("reflected lots qty=%d waiting=%d reserved=%d pending=%d", qty, waiting, reserved, pending)
	}
	if stock := productStock(t, productID); stock != 0 {
		t.Fatalf("reflection changed stock: %d", stock)
	}
	// The override is transaction-local and explicitly reset. A subsequent
	// ordinary addition must still append its own independently priced lot.
	addPriceLot(t, cartID, productID, 1, 0, 2000, "")
	var total int64
	if err := testPool.QueryRow(ctx, `SELECT cart_available_total_cents($1)`, cartID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 5101 {
		t.Fatalf("ordinary addition after reflection lost price: %d", total)
	}
}

func TestPriceLotsERPGridAcknowledgesOnlyMatchingPriceVector(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := tinyCheckoutProductionRepository(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	addPriceLot(t, cartID, productID, 2, 0, 1001, "")
	var external string
	if err := testPool.QueryRow(ctx, `SELECT external_id FROM products WHERE id=$1`, productID).Scan(&external); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=0 WHERE cart_id=$1`, cartID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		grid        []providers.ERPOrderItem
		wantPending bool
	}{
		{"wrong price despite exact quantity", []providers.ERPOrderItem{{ProductID: external, Quantity: 3, UnitPrice: 1001}}, true},
		{"all agreed prices", []providers.ERPOrderItem{{ProductID: external, Quantity: 1, UnitPrice: 1000}, {ProductID: external, Quantity: 2, UnitPrice: 1001}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := repo.ConfirmERPGrid(ctx, cartID, tc.grid); err != nil {
				t.Fatal(err)
			}
			var pending bool
			if err := testPool.QueryRow(ctx, `SELECT erp_pending_since IS NOT NULL FROM cart_items WHERE cart_id=$1`, cartID).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != tc.wantPending {
				t.Fatalf("pending=%v want=%v", pending, tc.wantPending)
			}
		})
	}
}

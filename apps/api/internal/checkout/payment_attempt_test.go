//go:build integration

package checkout

import (
	"context"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestPaymentQuote_ValidatesItemsEvenWhenTotalIsUnchanged(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cart := seedCartForPix(t)
	var store, integration, product string
	if err := testPool.QueryRow(ctx, `SELECT e.store_id::text FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1`, cart).Scan(&store); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'payment','pagarme','active') RETURNING id::text`, store).Scan(&integration); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO products(store_id,name,keyword,price,stock,external_source,external_id) VALUES($1,'Quote','1234',1000,10,'manual','quote') RETURNING id::text`, store).Scan(&product); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price) VALUES($1,$2,2,1000)`, cart, product); err != nil {
		t.Fatal(err)
	}
	items := []providers.CheckoutItem{{ID: product, Name: "Quote", Quantity: 2, UnitPrice: 1000}}
	id, err := testRepo.createPaymentAttempt(ctx, testPool, cart, integration, "pagarme", "pix", 2000, items)
	if err != nil || id == "" {
		t.Fatalf("valid quote: %s %v", id, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cart_items SET quantity=1,unit_price=2000 WHERE cart_id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	if _, err := testRepo.createPaymentAttempt(ctx, testPool, cart, integration, "pagarme", "pix", 2000, items); err == nil {
		t.Fatal("same-total item change accepted")
	}
	if _, err := testPool.Exec(ctx, `UPDATE carts SET payment_review_required=true WHERE id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	if _, err := testRepo.createPaymentAttempt(ctx, testPool, cart, integration, "pagarme", "pix", 2000, []providers.CheckoutItem{{ID: product, Quantity: 1, UnitPrice: 2000}}); err == nil {
		t.Fatal("reviewed payment allowed another charge")
	}
}

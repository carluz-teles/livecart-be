//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestPurchaseReflectionPreservesChildOriginsAndWaitingPrices(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	run(`UPDATE carts SET erp_order_state='reflecting' WHERE id=$1`, fx.cartID)
	// Host owns one available unit. Child owns one available + two waiting.
	run(`UPDATE cart_items SET quantity=3,waitlisted_quantity=2,erp_confirmed_quantity=1 WHERE cart_id=$1`, child)
	grid := []providers.ERPOrderItem{{ProductID: "123", Quantity: 2, UnitPrice: 900}}
	report, err := testRepo.reflectCartPurchase(t.Context(), fx.cartID, fx.storeID, "audit-host", grid, map[string]string{"123": fx.productID})
	if err != nil || len(report.Changes) != 1 {
		t.Fatalf("reflection: %+v %v", report, err)
	}
	for _, cart := range []string{fx.cartID, child} {
		var qty, price int
		if err := testPool.QueryRow(t.Context(), `SELECT SUM(l.quantity-l.waitlisted_quantity)::int,MIN(l.unit_price)::int FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id WHERE ci.cart_id=$1 AND l.quantity>l.waitlisted_quantity`, cart).Scan(&qty, &price); err != nil {
			t.Fatal(err)
		}
		if qty != 1 || price != 900 {
			t.Fatalf("cart attribution lost: cart=%s qty=%d price=%d", cart, qty, price)
		}
	}
	var waiting, price int
	if err := testPool.QueryRow(t.Context(), `SELECT SUM(l.waitlisted_quantity)::int,MIN(l.unit_price)::int FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id WHERE ci.cart_id=$1 AND l.waitlisted_quantity>0`, child).Scan(&waiting, &price); err != nil {
		t.Fatal(err)
	}
	if waiting != 2 || price != 1000 {
		t.Fatalf("waiting units repriced or fulfilled: qty=%d price=%d", waiting, price)
	}
	report, err = testRepo.reflectCartPurchase(t.Context(), fx.cartID, fx.storeID, "audit-host", grid, map[string]string{"123": fx.productID})
	if err != nil || len(report.Changes) != 0 {
		t.Fatalf("reflection is not idempotent: %+v %v", report, err)
	}
}

func TestPurchaseReflectionRollsBackAllProductsOnDatabaseFailure(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	run(`UPDATE carts SET erp_order_state='reflecting' WHERE id=$1`, fx.cartID)
	var second string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO products(store_id,name,keyword,external_source,external_id,price,stock) VALUES($1,'second','RB01','tiny','456',2000,4) RETURNING id::text`, fx.storeID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	run(`INSERT INTO cart_items(cart_id,product_id,quantity,unit_price) VALUES($1,$2,1,2000)`, child, second)
	run(fmt.Sprintf(`CREATE FUNCTION reflection_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.product_id='%s'::uuid AND NEW.unit_price IS DISTINCT FROM OLD.unit_price THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reflection_failure BEFORE UPDATE ON cart_items FOR EACH ROW EXECUTE FUNCTION reflection_failure()`, second))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reflection_failure ON cart_items; DROP FUNCTION IF EXISTS reflection_failure()`)
	})
	_, err := testRepo.reflectCartPurchase(t.Context(), fx.cartID, fx.storeID, "audit-host", []providers.ERPOrderItem{{ProductID: "123", Quantity: 2, UnitPrice: 900}, {ProductID: "456", Quantity: 1, UnitPrice: 1500}}, map[string]string{"123": fx.productID, "456": second})
	if err == nil {
		t.Fatal("injected database failure was ignored")
	}
	var changed int
	if err := testPool.QueryRow(t.Context(), `SELECT COUNT(*) FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id WHERE ci.cart_id=ANY($1::uuid[]) AND l.unit_price NOT IN (1000,2000)`, []string{fx.cartID, child}).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatal("partial reflection committed before second product failed")
	}
}

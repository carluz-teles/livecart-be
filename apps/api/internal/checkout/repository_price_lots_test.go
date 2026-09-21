//go:build integration

package checkout

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartpricing"
	"livecart/apps/api/internal/integration"
)

type priceLotFixture struct {
	cart, store, event, product string
}

func seedPriceLotCart(t *testing.T) priceLotFixture {
	t.Helper()
	requireDB(t)
	f := priceLotFixture{cart: seedCartForPix(t)}
	if err := testPool.QueryRow(t.Context(), `SELECT e.store_id::text,c.event_id::text
		FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1`, f.cart).Scan(&f.store, &f.event); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET store_id=$2 WHERE id=$1`, f.cart, f.store); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `INSERT INTO products
		(store_id,name,keyword,price,stock,external_source,external_id)
		VALUES($1,'Frozen price','4123',1001,0,'manual','price-lot') RETURNING id::text`, f.store).Scan(&f.product); err != nil {
		t.Fatal(err)
	}
	return f
}

func priceLotUUID(value string) pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.MustParse(value), Valid: true}
}

func addPriceLot(t *testing.T, f priceLotFixture, quantity, waiting int, price int64) string {
	t.Helper()
	item, err := testQueries.UpsertCartItem(t.Context(), sqlc.UpsertCartItemParams{
		CartID: priceLotUUID(f.cart), ProductID: priceLotUUID(f.product),
		Quantity:  pgtype.Int4{Int32: int32(quantity), Valid: true},
		UnitPrice: pgtype.Int8{Int64: price, Valid: true}, WaitlistedQuantity: int32(waiting),
	})
	if err != nil {
		t.Fatal(err)
	}
	return uuid.UUID(item.ID.Bytes).String()
}

func requestPriceLot(t *testing.T, f priceLotFixture, quantity int, price int64) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO waitlist_items
		(event_id,cart_id,product_id,platform_user_id,platform_handle,quantity,position,unit_price)
		SELECT c.event_id,c.id,$2,c.platform_user_id,c.platform_handle,$3,1,$4
		FROM carts c WHERE c.id=$1 RETURNING id::text`, f.cart, f.product, quantity, price).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func expectPriceLots(t *testing.T, f priceLotFixture, want []cartpricing.Lot, wantTotal int64) {
	t.Helper()
	items, err := testRepo.ListCartItems(t.Context(), f.cart)
	if err != nil || len(items) != 1 {
		t.Fatalf("checkout items=%+v err=%v", items, err)
	}
	if !reflect.DeepEqual(items[0].PriceLots, want) {
		t.Fatalf("checkout lots=%+v want=%+v", items[0].PriceLots, want)
	}
	var quantity, waiting int
	for _, lot := range want {
		quantity += lot.Quantity
		waiting += lot.WaitlistedQuantity
	}
	if items[0].Quantity != quantity || items[0].WaitlistedQuantity != waiting {
		t.Fatalf("cart split=(%d,%d), ledger split=(%d,%d)",
			items[0].Quantity, items[0].WaitlistedQuantity, quantity, waiting)
	}
	gotTotal, gotQuantity := CalculateCartTotal(items)
	if gotTotal != wantTotal || gotQuantity != quantity-waiting {
		t.Fatalf("payable=(%d,%d) want=(%d,%d)", gotTotal, gotQuantity, wantTotal, quantity-waiting)
	}
	var databaseTotal int64
	if err := testPool.QueryRow(t.Context(), `SELECT cart_available_total_cents($1)`, f.cart).Scan(&databaseTotal); err != nil {
		t.Fatal(err)
	}
	if databaseTotal != wantTotal {
		t.Fatalf("SQL payable=%d want=%d", databaseTotal, wantTotal)
	}
}

func TestPriceLots_NewRequestsAndPartialPromotionKeepAgreedPrices(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 3, 3, 1001)
	first := requestPriceLot(t, f, 3, 1001)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET price=2002 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	addPriceLot(t, f, 2, 2, 2002)
	requestPriceLot(t, f, 2, 2002)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET price=9999,stock=1 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	r := integration.NewRepository(testQueries, testPool)
	promoted, err := r.PromoteNextWaitlistEntry(t.Context(), f.store, f.product)
	if err != nil || promoted == nil || promoted.WaitlistItemID != first || promoted.Quantity != 1 || promoted.Remaining != 2 {
		t.Fatalf("partial promotion=%+v err=%v", promoted, err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 2, WaitlistedQuantity: 2, UnitPrice: 2002},
	}, 1001)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=4 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.PromoteNextWaitlistEntry(t.Context(), f.store, f.product); err != nil {
			t.Fatal(err)
		}
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 3, UnitPrice: 1001, TotalPrice: 3003},
		{Quantity: 2, UnitPrice: 2002, TotalPrice: 4004},
	}, 7007)
}

func TestPriceLots_ReductionCancelsNewestWaitingThenNewestAvailable(t *testing.T) {
	f := seedPriceLotCart(t)
	item := addPriceLot(t, f, 3, 2, 1001)
	first := requestPriceLot(t, f, 2, 1001)
	addPriceLot(t, f, 4, 3, 2002)
	second := requestPriceLot(t, f, 3, 2002)
	changed, err := testRepo.SetCartItemSplitIfUnchanged(t.Context(), item, 7, 5, 3)
	if err != nil || !changed {
		t.Fatalf("reduce newest waiting: changed=%v err=%v", changed, err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 2, WaitlistedQuantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}, 3003)
	for id, want := range map[string]int{first: 2, second: 1} {
		var remaining int
		if err := testPool.QueryRow(t.Context(), `SELECT quantity FROM waitlist_items WHERE id=$1`, id).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != want {
			t.Fatalf("request %s remaining=%d want=%d", id, remaining, want)
		}
	}
	changed, err = testRepo.SetCartItemSplitIfUnchanged(t.Context(), item, 5, 1, 0)
	if err != nil || !changed {
		t.Fatalf("reduce available: changed=%v err=%v", changed, err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{{Quantity: 1, UnitPrice: 1001, TotalPrice: 1001}}, 1001)
}

func TestPriceLots_LeavingOlderRequestKeepsOtherPricesAndPromotedUnits(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 3, 2, 1001)
	first := requestPriceLot(t, f, 2, 1001)
	addPriceLot(t, f, 2, 2, 2002)
	second := requestPriceLot(t, f, 2, 2002)
	r := integration.NewRepository(testQueries, testPool)
	changed, err := r.CancelWaitingRequest(t.Context(), first, f.cart)
	if err != nil || !changed {
		t.Fatalf("leave first request: changed=%v err=%v", changed, err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 2, WaitlistedQuantity: 2, UnitPrice: 2002},
	}, 1001)
	changed, err = r.CancelWaitingRequest(t.Context(), first, f.cart)
	if err != nil || changed {
		t.Fatalf("duplicate leave changed state: changed=%v err=%v", changed, err)
	}
	var status string
	var quantity int
	if err := testPool.QueryRow(t.Context(), `SELECT status,quantity FROM waitlist_items WHERE id=$1`, second).Scan(&status, &quantity); err != nil {
		t.Fatal(err)
	}
	if status != "waiting" || quantity != 2 {
		t.Fatalf("newer request changed: %s %d", status, quantity)
	}
}

func TestPriceLots_PaymentCancelsRemainingRequestsWithoutChangingPayableUnits(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 3, 2, 1001)
	requestPriceLot(t, f, 2, 1001)
	addPriceLot(t, f, 2, 1, 2002)
	requestPriceLot(t, f, 1, 2002)
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(t.Context(), `UPDATE carts SET payment_status='paid' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	var waiting, cancelled, quantity int
	if err := tx.QueryRow(t.Context(), `SELECT COUNT(*) FILTER (WHERE status='waiting'),
		COALESCE(SUM(cancelled_quantity),0),COALESCE(SUM(quantity),0)
		FROM waitlist_items WHERE cart_id=$1`, f.cart).Scan(&waiting, &cancelled, &quantity); err != nil {
		t.Fatal(err)
	}
	if waiting != 0 || cancelled != 3 || quantity != 0 {
		t.Fatalf("waiting after payment=(%d,%d,%d)", waiting, cancelled, quantity)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}, 3003)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=99 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	r := integration.NewRepository(testQueries, testPool)
	if promoted, err := r.PromoteNextWaitlistEntry(t.Context(), f.store, f.product); err != nil || promoted != nil {
		t.Fatalf("paid cart received later restock: %+v %v", promoted, err)
	}
}

func TestPriceLots_MergePreservesPricesAndWaitingRequests(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 2, 1, 1001)
	first := requestPriceLot(t, f, 1, 1001)
	var handle string
	if err := testPool.QueryRow(t.Context(), `UPDATE carts SET created_at=now()-interval '1 day'
		WHERE id=$1 RETURNING platform_handle`, f.cart).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	dest := priceLotFixture{store: f.store, product: f.product}
	if err := testPool.QueryRow(t.Context(), `INSERT INTO live_events(store_id,status,title,ends_at)
		VALUES($1,'active','Merge destination',now()+interval '1 day') RETURNING id::text`, f.store).Scan(&dest.event); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts
		(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,payment_status)
		SELECT $2,store_id,platform_user_id,platform_handle,token||'-dest',short_id+1,'checkout','unpaid'
		FROM carts WHERE id=$1 RETURNING id::text`, f.cart, dest.event).Scan(&dest.cart); err != nil {
		t.Fatal(err)
	}
	addPriceLot(t, dest, 3, 1, 2002)
	requestPriceLot(t, dest, 1, 2002)
	r := integration.NewRepository(testQueries, testPool)
	merged, err := r.ConsolidateEternalCartForHandle(t.Context(), f.store, handle)
	if err != nil || merged.EternalCartID != dest.cart || len(merged.MergedCartIDs) != 1 {
		t.Fatalf("VIP merge=%+v err=%v", merged, err)
	}
	expectPriceLots(t, dest, []cartpricing.Lot{
		{Quantity: 2, WaitlistedQuantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 3, WaitlistedQuantity: 1, UnitPrice: 2002, TotalPrice: 4004},
	}, 5005)
	var owner, status string
	var quantity int
	if err := testPool.QueryRow(t.Context(), `SELECT cart_id::text,status,quantity
		FROM waitlist_items WHERE id=$1`, first).Scan(&owner, &status, &quantity); err != nil {
		t.Fatal(err)
	}
	if owner != dest.cart || status != "waiting" || quantity != 1 {
		t.Fatalf("merged request=(%s,%s,%d)", owner, status, quantity)
	}
	if changed, err := r.CancelWaitingRequest(t.Context(), first, dest.cart); err != nil || !changed {
		t.Fatalf("leave transferred lot: changed=%v err=%v", changed, err)
	}
	expectPriceLots(t, dest, []cartpricing.Lot{
		{Quantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 3, WaitlistedQuantity: 1, UnitPrice: 2002, TotalPrice: 4004},
	}, 5005)
}

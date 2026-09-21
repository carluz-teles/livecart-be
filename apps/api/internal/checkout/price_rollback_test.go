//go:build integration

package checkout

import (
	"reflect"
	"testing"

	"livecart/apps/api/internal/cartpricing"
	"livecart/apps/api/internal/integration"
)

func mutateForRollbackTest(t *testing.T, before *CartItemRow, quantity int, deleted bool) *cartItemMutationState {
	t.Helper()
	svc := &Service{pool: testPool}
	state, err := svc.mutateCartItemWithSnapshot(t.Context(), before, func(repo *Repository) error {
		if deleted {
			return repo.DeleteCartItem(t.Context(), before.ID)
		}
		_, err := repo.SetCartItemSplitIfUnchanged(t.Context(), before.ID, before.Quantity, quantity, before.WaitlistedQuantity)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRestoreCartItem_PreservesEveryAgreedPriceAfterERPFailure(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "quantity reduction"
		if deleted {
			name = "item removal"
		}
		t.Run(name, func(t *testing.T) {
			f := seedPriceLotCart(t)
			id := addPriceLot(t, f, 2, 0, 1001)
			addPriceLot(t, f, 2, 0, 2002)
			before, err := testRepo.GetCartItem(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			expected := mutateForRollbackTest(t, before, 2, deleted)
			svc := &Service{pool: testPool}
			if err := svc.restoreCartItem(t.Context(), before, expected); err != nil {
				t.Fatal(err)
			}
			expectPriceLots(t, f, []cartpricing.Lot{
				{Quantity: 2, UnitPrice: 1001, TotalPrice: 2002},
				{Quantity: 2, UnitPrice: 2002, TotalPrice: 4004},
			}, 6006)
			after, err := testRepo.GetCartItem(t.Context(), id)
			if err != nil || after.ID != before.ID || !reflect.DeepEqual(after.PriceLots, before.PriceLots) {
				t.Fatalf("restored identity/price agreements changed: %+v, %v", after, err)
			}
		})
	}
}

func TestRestoreCartItem_DoesNotOverwriteNewerQuantity(t *testing.T) {
	f := seedPriceLotCart(t)
	id := addPriceLot(t, f, 2, 0, 1001)
	addPriceLot(t, f, 2, 0, 2002)
	before, err := testRepo.GetCartItem(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	expected := mutateForRollbackTest(t, before, 2, false)
	addPriceLot(t, f, 1, 0, 3003)
	svc := &Service{pool: testPool}
	if err := svc.restoreCartItem(t.Context(), before, expected); err == nil {
		t.Fatal("old compensation overwrote a later addition")
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 2, UnitPrice: 1001, TotalPrice: 2002},
		{Quantity: 1, UnitPrice: 3003, TotalPrice: 3003},
	}, 5005)
}

func TestRestoreCartItem_DoesNotRestoreUnitsAfterPayment(t *testing.T) {
	f := seedPriceLotCart(t)
	id := addPriceLot(t, f, 2, 0, 1001)
	before, err := testRepo.GetCartItem(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	expected := mutateForRollbackTest(t, before, 1, false)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='paid' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	svc := &Service{pool: testPool}
	if err := svc.restoreCartItem(t.Context(), before, expected); err == nil {
		t.Fatal("compensation changed a paid purchase")
	}
	expectPriceLots(t, f, []cartpricing.Lot{{Quantity: 1, UnitPrice: 1001, TotalPrice: 1001}}, 1001)
}

func TestRestoreCartItem_DoesNotUndoPromotionDuringERPFailure(t *testing.T) {
	f := seedPriceLotCart(t)
	id := addPriceLot(t, f, 2, 1, 1001)
	request := requestPriceLot(t, f, 1, 1001)
	before, err := testRepo.GetCartItem(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	expected := mutateForRollbackTest(t, before, 3, false)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=1 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	repo := integration.NewRepository(testQueries, testPool)
	promotion, err := repo.PromoteNextWaitlistEntry(t.Context(), f.store, f.product)
	if err != nil || promotion == nil || promotion.WaitlistItemID != request || promotion.Quantity != 1 {
		t.Fatalf("promotion=%+v err=%v", promotion, err)
	}
	svc := &Service{pool: testPool}
	if err := svc.restoreCartItem(t.Context(), before, expected); err == nil {
		t.Fatal("compensation restored waiting units that had already been promoted")
	}
	after, err := testRepo.GetCartItem(t.Context(), id)
	if err != nil || after.Quantity != 3 || after.WaitlistedQuantity != 0 {
		t.Fatalf("promoted purchase changed: %+v err=%v", after, err)
	}
	var stock, remaining, promoted int
	if err := testPool.QueryRow(t.Context(), `SELECT p.stock,
        CASE WHEN w.status='waiting' THEN w.quantity ELSE 0 END,w.fulfilled_quantity
        FROM products p JOIN waitlist_items w ON w.product_id=p.id WHERE p.id=$1 AND w.id=$2`,
		f.product, request).Scan(&stock, &remaining, &promoted); err != nil {
		t.Fatal(err)
	}
	if stock != 0 || remaining != 0 || promoted != 1 {
		t.Fatalf("promotion changed: stock=%d remaining=%d promoted=%d", stock, remaining, promoted)
	}
}

func TestRestoreCartItem_PreservesSameQuantityEditsAndFinancialChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"price lot correction", `UPDATE cart_item_price_lots SET unit_price=3003 WHERE cart_item_id=$1`},
		{"paid units", `UPDATE cart_items SET paid_quantity=1 WHERE id=$1`},
		{"payment review", `UPDATE carts SET payment_review_required=true WHERE id=(SELECT cart_id FROM cart_items WHERE id=$1)`},
		{"partial payment", `UPDATE carts SET paid_amount_cents=1001 WHERE id=(SELECT cart_id FROM cart_items WHERE id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedPriceLotCart(t)
			id := addPriceLot(t, f, 2, 0, 1001)
			before, err := testRepo.GetCartItem(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			expected := mutateForRollbackTest(t, before, 3, false)
			if _, err := testPool.Exec(t.Context(), tc.sql, id); err != nil {
				t.Fatal(err)
			}
			concurrent, err := testRepo.GetCartItem(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			svc := &Service{pool: testPool}
			if err := svc.restoreCartItem(t.Context(), before, expected); err == nil {
				t.Fatal("compensation overwrote a concurrent change with the same total quantity")
			}
			after, err := testRepo.GetCartItem(t.Context(), id)
			if err != nil || !reflect.DeepEqual(concurrent, after) {
				t.Fatalf("concurrent state changed: before=%+v after=%+v err=%v", concurrent, after, err)
			}
		})
	}
}

func TestRestoreCartItem_NewItemPreservesConcurrentChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"unchanged addition is removed", ""},
		{"new price survives", `UPDATE cart_item_price_lots SET unit_price=3003 WHERE cart_item_id=$1`},
		{"paid units survive", `UPDATE cart_items SET paid_quantity=1 WHERE id=$1`},
		{"payment review survives", `UPDATE carts SET payment_review_required=true WHERE id=(SELECT cart_id FROM cart_items WHERE id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedPriceLotCart(t)
			svc := &Service{pool: testPool}
			expected, err := svc.createCartItemWithSnapshot(t.Context(), f.cart, f.product, 1, 1001)
			if err != nil {
				t.Fatal(err)
			}
			if tc.sql != "" {
				if _, err := testPool.Exec(t.Context(), tc.sql, expected.item.ID); err != nil {
					t.Fatal(err)
				}
			}
			err = svc.restoreCartItem(t.Context(), nil, expected)
			if (err != nil) != (tc.sql != "") {
				t.Fatalf("restore error=%v, concurrent=%v", err, tc.sql != "")
			}
			var exists bool
			if err := testPool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM cart_items WHERE id=$1)`, expected.item.ID).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != (tc.sql != "") {
				t.Fatalf("item survives=%v, concurrent=%v", exists, tc.sql != "")
			}
		})
	}
}

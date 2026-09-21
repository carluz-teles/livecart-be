//go:build integration

package checkout

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"livecart/apps/api/internal/cartedit"

	"livecart/apps/api/internal/cartpricing"
)

func TestAddCartItemQuantityAtPrice_PreservesOlderLotsAndRejectsStaleQuantity(t *testing.T) {
	f := seedPriceLotCart(t)
	id := addPriceLot(t, f, 2, 1, 1001)
	changed, err := testRepo.AddCartItemQuantityAtPrice(t.Context(), id, 2, 1, 2002)
	if err != nil || !changed {
		t.Fatalf("addition changed=%v err=%v", changed, err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 2, WaitlistedQuantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}, 3003)
	changed, err = testRepo.AddCartItemQuantityAtPrice(t.Context(), id, 2, 5, 9999)
	if err != nil || changed {
		t.Fatalf("stale addition changed=%v err=%v", changed, err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 2, WaitlistedQuantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}, 3003)
}

func TestAddCartItemQuantityAtPrice_ConcurrentExpectedQuantityHasOneWinner(t *testing.T) {
	f := seedPriceLotCart(t)
	id := addPriceLot(t, f, 2, 0, 1001)
	type result struct {
		changed bool
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, price := range []int64{2002, 3003} {
		wg.Add(1)
		go func(price int64) {
			defer wg.Done()
			changed, err := testRepo.AddCartItemQuantityAtPrice(t.Context(), id, 2, 1, price)
			results <- result{changed, err}
		}(price)
	}
	wg.Wait()
	close(results)
	wins := 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.changed {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent expected-quantity updates had %d winners", wins)
	}
	item, err := testRepo.GetCartItem(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Quantity != 3 || len(item.PriceLots) != 2 || item.PriceLots[0].Quantity != 2 || item.PriceLots[0].UnitPrice != 1001 {
		t.Fatalf("concurrent addition lost or repriced older units: %+v", item)
	}
}

func TestAddCartItemQuantityAtPrice_CompensationRestoresSnapshotWithoutErasingNewerAddition(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "restore original price lots"
		if concurrent {
			name = "preserve newer addition"
		}
		t.Run(name, func(t *testing.T) {
			f := seedPriceLotCart(t)
			id := addPriceLot(t, f, 2, 0, 1001)
			before, err := testRepo.GetCartItem(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			svc := &Service{pool: testPool}
			expected, err := svc.mutateCartItemWithSnapshot(t.Context(), before, func(repo *Repository) error {
				_, err := repo.AddCartItemQuantityAtPrice(t.Context(), id, 2, 1, 2002)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if concurrent {
				if changed, err := testRepo.AddCartItemQuantityAtPrice(t.Context(), id, 3, 1, 3003); err != nil || !changed {
					t.Fatalf("newer addition %v %v", changed, err)
				}
			}
			err = svc.restoreCartItem(t.Context(), before, expected)
			if concurrent {
				if err == nil {
					t.Fatal("old compensation overwrote a newer addition")
				}
				expectPriceLots(t, f, []cartpricing.Lot{
					{Quantity: 2, UnitPrice: 1001, TotalPrice: 2002},
					{Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
					{Quantity: 1, UnitPrice: 3003, TotalPrice: 3003},
				}, 7007)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				expectPriceLots(t, f, []cartpricing.Lot{{Quantity: 2, UnitPrice: 1001, TotalPrice: 2002}}, 2002)
			}
		})
	}
}

func TestMerchantAdditionAtPrice_UsesNewQuoteAndKeepsIdempotency(t *testing.T) {
	f := seedMerchantEdit(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET price=2002 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	for range 2 {
		if err := f.service.AddCartItemAsMerchant(ctx, f.token, f.product, 1); err != nil {
			t.Fatal(err)
		}
	}
	expectPriceLots(t, priceLotFixture{cart: f.cart}, []cartpricing.Lot{
		{Quantity: 2, UnitPrice: 1000, TotalPrice: 2000}, {Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}, 4002)
	var stock, requests int
	if err := testPool.QueryRow(t.Context(), `SELECT stock,(SELECT count(*) FROM cart_erp_edit_requests WHERE cart_id=$2)
	    FROM products WHERE id=$1`, f.product, f.cart).Scan(&stock, &requests); err != nil {
		t.Fatal(err)
	}
	if stock != 7 || requests != 1 {
		t.Fatalf("duplicate request changed inventory: stock=%d requests=%d", stock, requests)
	}
}

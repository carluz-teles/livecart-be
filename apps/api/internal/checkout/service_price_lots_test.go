//go:build integration

package checkout

import (
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/cartpricing"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/lib/httpx"
)

func TestPublicCartAdd_ExistingProductUsesCurrentPriceOnlyForNewUnits(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 1, 0, 1001)
	if _, err := testPool.Exec(t.Context(), `UPDATE stores SET cart_allow_edit=true WHERE id=$1`, f.store); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET price=2002,stock=5 WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := testPool.QueryRow(t.Context(), `SELECT token FROM carts WHERE id=$1`, f.cart).Scan(&token); err != nil {
		t.Fatal(err)
	}
	// A manual catalog exercises the real reservation flow without an external ERP.
	integrations := integration.NewService(
		integration.NewRepository(testQueries, testPool), nil, nil, nil, nil, zap.NewNop(),
	)
	svc := NewService(testRepo, testPool, integrations, nil, zap.NewNop())
	if _, err := svc.AddCartItem(t.Context(), MutateCartItemInput{
		Token: token, ProductID: f.product, Quantity: 1,
	}); err != nil {
		t.Fatal(err)
	}
	expectPriceLots(t, f, []cartpricing.Lot{
		{Quantity: 1, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}, 3003)
	var remaining int
	if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 4 {
		t.Fatalf("remaining stock=%d want=4", remaining)
	}
}

func TestPublicCartEdit_WithWaitingRequiresExplicitExitBeforeChangingAvailableUnits(t *testing.T) {
	for _, action := range []string{"quantity", "remove"} {
		t.Run(action, func(t *testing.T) {
			f := seedPriceLotCart(t)
			item := addPriceLot(t, f, 3, 2, 1001)
			requestPriceLot(t, f, 2, 1001)
			if _, err := testPool.Exec(t.Context(), `UPDATE stores SET cart_allow_edit=true WHERE id=$1`, f.store); err != nil {
				t.Fatal(err)
			}
			var token string
			if err := testPool.QueryRow(t.Context(), `SELECT token FROM carts WHERE id=$1`, f.cart).Scan(&token); err != nil {
				t.Fatal(err)
			}
			// No ERP/gateway is connected. Rejection must happen before any external effect.
			svc := NewService(testRepo, testPool, nil, nil, zap.NewNop())
			input := MutateCartItemInput{Token: token, ItemID: item, Quantity: 2}
			var err error
			if action == "quantity" {
				_, err = svc.UpdateCartItemQuantity(t.Context(), input)
			} else {
				_, err = svc.RemoveCartItem(t.Context(), input)
			}
			var domain *httpx.ServiceError
			if !errors.As(err, &domain) || domain.Code != 409 || !strings.Contains(domain.Message, "espera") {
				t.Fatalf("expected explicit queue-exit conflict, got %v", err)
			}
			expectPriceLots(t, f, []cartpricing.Lot{
				{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001, TotalPrice: 1001},
			}, 1001)
		})
	}
}

package checkout

import (
	"encoding/json"
	"reflect"
	"testing"

	"livecart/apps/api/internal/cartpricing"
)

func TestCartResponse_PriceLotsExcludeWaitingAndKeepExactTotals(t *testing.T) {
	lots := []cartpricing.Lot{
		{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001, TotalPrice: 1001},
		{Quantity: 1, UnitPrice: 2002, TotalPrice: 2002},
	}
	handler := &Handler{}
	response := handler.toCartResponse(&GetCartForCheckoutOutput{
		Cart: CartDetails{Status: "checkout"},
		Items: []CartItemDetails{{
			ID: "item", ProductID: "product", Quantity: 4, WaitlistedQuantity: 2,
			UnitPrice: 9999, PriceLots: lots,
		}},
	})
	if response.Summary.Subtotal != 3003 || response.Summary.Total != 3003 || response.Summary.TotalItems != 2 {
		t.Fatalf("public summary=%+v", response.Summary)
	}
	if len(response.Items) != 1 || response.Items[0].TotalPrice != 3003 || !reflect.DeepEqual(response.Items[0].PriceLots, lots) {
		t.Fatalf("public item discarded price agreements: %+v", response.Items)
	}
	raw, err := json.Marshal(response.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		PriceLots []cartpricing.Lot `json:"priceLots"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || !reflect.DeepEqual(wire.PriceLots, lots) {
		t.Fatalf("frontend priceLots contract mismatch: %s, %v", raw, err)
	}
}

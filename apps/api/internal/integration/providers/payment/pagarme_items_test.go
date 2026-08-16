package payment

import "testing"

// TestPagarmeOrderItems guards the coupon/shipping reconciliation. Pagar.me
// derives order.amount from sum(item.amount*quantity) AND rejects any item with
// amount < 1 — so a discount can't be a negative line (the 422 that broke PIX
// on coupon orders). Every case must end with sum(items) == totalAmount and no
// item below 1.
func TestPagarmeOrderItems(t *testing.T) {
	sumItems := func(items []map[string]any) int64 {
		var total int64
		for _, it := range items {
			amount := it["amount"].(int64)
			if amount < 1 {
				t.Fatalf("item amount %d < 1 (Pagar.me rejects it): %v", amount, it)
			}
			total += amount * int64(it["quantity"].(int))
		}
		return total
	}

	tests := []struct {
		name  string
		items []CheckoutItem
		total int64
	}{
		{
			name:  "exact — no adjustment",
			items: []CheckoutItem{{ID: "a", Name: "A", UnitPrice: 490, Quantity: 1}},
			total: 490,
		},
		{
			name:  "surcharge (frete) stays a positive line",
			items: []CheckoutItem{{ID: "a", Name: "A", UnitPrice: 490, Quantity: 1}},
			total: 520,
		},
		{
			name:  "discount on single unit — the real BO (490 -> 466)",
			items: []CheckoutItem{{ID: "b21e5d0a", Name: "Sacola", UnitPrice: 490, Quantity: 1}},
			total: 466,
		},
		{
			name:  "discount divides evenly across quantity",
			items: []CheckoutItem{{ID: "a", Name: "A", UnitPrice: 490, Quantity: 3}},
			total: 1470 - 24,
		},
		{
			name:  "discount with remainder splits a unit",
			items: []CheckoutItem{{ID: "a", Name: "A", UnitPrice: 490, Quantity: 3}},
			total: 1470 - 25,
		},
		{
			name: "discount across multiple products",
			items: []CheckoutItem{
				{ID: "a", Name: "A", UnitPrice: 490, Quantity: 2},
				{ID: "b", Name: "B", UnitPrice: 300, Quantity: 1},
			},
			total: 1280 - 37,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := pagarmeOrderItems(tt.items, tt.total)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := sumItems(out); got != tt.total {
				t.Errorf("sum(items) = %d, want %d", got, tt.total)
			}
			// Codes sent to Pagar.me must be unique.
			seen := map[string]bool{}
			for _, it := range out {
				code := it["code"].(string)
				if seen[code] {
					t.Errorf("duplicate item code %q", code)
				}
				seen[code] = true
			}
		})
	}
}

// TestPagarmeOrderItemsDiscountTooLarge: a discount that can't be absorbed
// (would push items below 1) is an error, not a silently wrong charge.
func TestPagarmeOrderItemsDiscountTooLarge(t *testing.T) {
	items := []CheckoutItem{{ID: "a", Name: "A", UnitPrice: 2, Quantity: 1}}
	if _, err := pagarmeOrderItems(items, 0); err == nil {
		t.Fatal("expected error when discount exceeds reducible item value")
	}
}

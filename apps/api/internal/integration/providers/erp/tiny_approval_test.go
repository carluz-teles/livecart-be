package erp

import (
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyApprovalSnapshotReadsCurrentSaleWithoutWrites(t *testing.T) {
	provider, fake, _ := consistentFinalizedTinyOrder(t)
	got, err := provider.GetOrderApprovalSnapshot(t.Context(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.OrderID != "1" || got.Status != providers.ERPOrderStatusFaturado ||
		got.TotalCents != 6599 || got.FreightCents != 1859 || got.InvoiceID != "99" ||
		len(got.Items) != 1 || got.Items[0].ProductID != "20" || got.Items[0].UnitPrice != 4990 {
		t.Fatalf("incorrect sale snapshot: %+v", got)
	}
	if fake.writes != 0 || fake.accountReads != 0 {
		t.Fatalf("approval used unrelated operations: writes=%d accounts=%d", fake.writes, fake.accountReads)
	}
}

func TestTinyApprovalSnapshotRejectsIncompleteOrDifferentSale(t *testing.T) {
	for _, scenario := range []string{"wrong id", "unknown status", "missing items", "missing product", "zero quantity", "negative price", "zero total", "negative freight", "deleted"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, _ := consistentFinalizedTinyOrder(t)
			order := fake.orders["1"]
			switch scenario {
			case "wrong id":
				order.ID = 2
			case "unknown status":
				order.Status = 999
			case "missing items":
				order.Items = nil
			case "missing product":
				order.Items[0].Product.ID = 0
			case "zero quantity":
				order.Items[0].Quantity = 0
			case "negative price":
				order.Items[0].UnitPrice = -1
			case "zero total":
				order.Total = 0
			case "negative freight":
				order.Freight = -1
			case "deleted":
				delete(fake.orders, "1")
			}
			if got, err := provider.GetOrderApprovalSnapshot(t.Context(), "1"); err == nil || got != nil {
				t.Fatalf("invalid snapshot accepted: %+v %v", got, err)
			}
			if fake.writes != 0 {
				t.Fatal("invalid snapshot caused an ERP write")
			}
		})
	}
}

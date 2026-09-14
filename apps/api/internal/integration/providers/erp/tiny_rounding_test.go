package erp

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyExistingCheckoutRecognizesCompensatedRounding(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		products, freight, discount, expenses, total float64
		discountCents, paidCents                     int64
		wantConflict                                 bool
	}{
		{"small PIX sale", 55.90, 15.16, 2.795, 0.01, 68.27, 279, 6827, false},
		{"larger PIX sale", 544.70, 30.57, 27.235000000000003, 0.01, 548.04, 2723, 54804, false},
		{"uncompensated cent", 55.90, 15.16, 2.79, 0.01, 68.28, 279, 6827, true},
		{"different total", 55.90, 15.16, 2.795, 0.01, 68.28, 279, 6827, true},
		{"larger expense", 55.90, 15.16, 2.895, 0.11, 68.27, 279, 6827, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fake, op := consistentFinalizedTinyOrder(t)
			source := fake.orders["1"]
			source.Freight, source.Discount, source.OtherExpenses, source.Total = tc.freight, tc.discount, tc.expenses, tc.total
			source.Items[0].UnitPrice = tc.products
			op.Order.Items[0].UnitPrice = int64(tc.products*100 + 0.5)
			op.Order.Checkout.FreightCents = int64(tc.freight*100 + 0.5)
			op.Order.Checkout.DiscountCents = tc.discountCents
			op.Order.Checkout.Payments[0].AmountCents = tc.paidCents
			source.Payment.Installments[0].Value = float64(tc.paidCents) / 100
			fields := existingTinyCheckoutDifferences(source, op, 0)
			if (len(fields) > 0) != tc.wantConflict {
				t.Fatalf("unexpected financial differences: %v", fields)
			}
			if source.Discount != tc.discount || source.OtherExpenses != tc.expenses {
				t.Fatal("comparison changed the ERP snapshot")
			}
			if !tc.wantConflict && !slices.Contains(tinyCheckoutDifferences(source, *op.Order.Checkout), "desconto") {
				t.Fatal("replacement verification must still enforce the original discount")
			}
			if source.Status != providers.SituacaoFaturada {
				t.Fatal("comparison changed the order status")
			}
		})
	}
}

func TestTinyReusesCorrectedPIXSoldOrderWithStockAndAccounts(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		products, freight, discount, total float64
		discountCents                      int64
	}{
		{"reported 1492 shape", 55.90, 15.16, 2.795, 68.27, 279},
		{"reported 1487 shape", 544.70, 30.57, 27.235000000000003, 548.04, 2723},
	} {
		for _, resume := range []bool{false, true} {
			name := tc.name
			if resume {
				name += " bind retry"
			}
			t.Run(name, func(t *testing.T) {
				provider, fake, op := consistentFinalizedTinyOrder(t)
				source := fake.orders["1"]
				source.Status, source.InvoiceID = 0, 0
				source.Freight, source.Discount, source.OtherExpenses, source.Total = tc.freight, tc.discount, 0.01, tc.total
				source.Items[0].UnitPrice = tc.products
				op.Order.Items[0].UnitPrice = int64(tc.products*100 + 0.5)
				op.Order.Checkout.FreightCents, op.Order.Checkout.DiscountCents = int64(tc.freight*100+0.5), tc.discountCents
				op.Order.Checkout.Payments[0].AmountCents = int64(tc.total*100 + 0.5)
				op.Order.Checkout.Payments[0].DueDate = time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
				source.Payment.Installments[0].Value, source.Payment.Installments[0].Date = tc.total, "2026-09-12"
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: tc.total, Balance: tc.total, DueDate: "2026-09-14"}}
				fake.stockLocked = true
				beforeOrder, err := json.Marshal(source)
				if err != nil {
					t.Fatal(err)
				}
				beforeAccounts := slices.Clone(fake.accounts["1"])
				journal := &checkoutTestJournal{}
				if resume {
					journal.bindFailures = 1
				}
				if err := journal.Save(t.Context(), op); err != nil {
					t.Fatal(err)
				}
				_, err = provider.FinalizePaidCheckout(t.Context(), op, journal)
				if resume {
					if err == nil {
						t.Fatal("expected interrupted local bind")
					}
					op = journal.resume(t)
					_, err = provider.FinalizePaidCheckout(t.Context(), op, journal)
				}
				if err != nil || journal.bound != "1" || !op.Completed || op.TargetStatus != providers.ERPOrderStatusAprovado {
					t.Fatalf("existing paid sale was not completed: err=%v op=%+v", err, op)
				}
				if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
					t.Fatal(err)
				}
				if fake.writes != 1 || fake.posts != 0 || fake.cancels != 0 || fake.reversals != 0 {
					t.Fatalf("only approval was allowed: writes=%d posts=%d cancels=%d reversals=%d", fake.writes, fake.posts, fake.cancels, fake.reversals)
				}
				if !reflect.DeepEqual(beforeAccounts, fake.accounts["1"]) {
					t.Fatal("financial entries changed")
				}
				var expected tinyCheckoutOrder
				if err := json.Unmarshal(beforeOrder, &expected); err != nil {
					t.Fatal(err)
				}
				expected.Status = providers.SituacaoAprovada
				if !reflect.DeepEqual(&expected, source) {
					t.Fatal("order fields other than approval changed")
				}
			})
		}
	}
}

func TestTinyPreservesPIXReceivableDatesOnlyForVerifiedAmounts(t *testing.T) {
	due := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"PIX", "received PIX", "card", "wrong amount", "missing", "duplicate", "cancelled", "unknown status", "invalid due date", "negative balance"} {
		t.Run(scenario, func(t *testing.T) {
			payments := []providers.ERPInstallment{{AmountCents: 6827, Method: "pix", DueDate: due}}
			accounts := []tinyReceivable{{ID: 50, Status: "aberto", Value: 68.27, Balance: 68.27, DueDate: "2026-09-14"}}
			switch scenario {
			case "received PIX":
				accounts[0].Status, accounts[0].Balance = "pago", 0
			case "card":
				payments[0].Method = "credit_card"
			case "wrong amount":
				accounts[0].Value = 68.26
			case "missing":
				accounts = nil
			case "duplicate":
				accounts = append(accounts, accounts[0])
			case "cancelled":
				accounts[0].Status = "cancelada"
			case "unknown status":
				accounts[0].Status = "unknown"
			case "invalid due date":
				accounts[0].DueDate = "2026-99-99"
			case "negative balance":
				accounts[0].Balance = -1
			}
			want := scenario == "PIX" || scenario == "received PIX"
			if tinyExistingCheckoutReceivablesMatch(accounts, payments) != want {
				t.Fatal("unexpected reconciliation decision")
			}
			if tinyReceivablesMatch(accounts, payments) {
				t.Fatal("strict financial write verification was weakened")
			}
		})
	}
}

func TestTinyCorrectedPIXStillBlocksAnIncorrectTotal(t *testing.T) {
	provider, fake, op := consistentFinalizedTinyOrder(t)
	fake.orders["1"].Total += 0.01
	journal := &checkoutTestJournal{}
	_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
	var conflict *providers.TinyCheckoutReconciliationError
	if !errors.As(err, &conflict) || !slices.Contains(conflict.Fields, "total pago/desconto") || journal.bound != "" || fake.writes != 0 {
		t.Fatalf("incorrect paid total accepted: %v", err)
	}
}

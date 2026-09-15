package erp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyCheckoutStockClassifiesOnlyExplicitRejections(t *testing.T) {
	for _, tc := range []struct {
		name, action, body  string
		status              int
		success, notApplied bool
	}{
		{"already launched", "lancar-estoque", `{"mensagem":"Estoque já lançado."}`, 400, true, false},
		{"insufficient stock", "lancar-estoque", `{"mensagem":"Estoque insuficiente."}`, 400, false, true},
		{"reversal rejection", "estornar-estoque", `{"mensagem":"Estoque já lançado."}`, 400, false, true},
		{"ambiguous server failure", "lancar-estoque", `{}`, 503, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := newTinyAgainst(t, srv).checkoutStockRequest(t.Context(), "1", tc.action)
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var stockErr *tinyStockRequestError
				if !errors.As(err, &stockErr) || stockErr.notApplied != tc.notApplied {
					t.Fatalf("incorrect stock result classification: %v", err)
				}
			}
			if calls != 1 {
				t.Fatalf("unsafe stock retry: %d calls", calls)
			}
		})
	}
}

func TestTinyPaidStockRecoveryDoesNotRepeatUncertainMovements(t *testing.T) {
	for _, scenario := range []string{"lost reversal response", "lost reversal checkpoint", "lost launch response", "lost launch checkpoint", "reversal rejected", "launch rejected"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			fake.stockLocked = true
			journal := &checkoutTestJournal{}
			switch scenario {
			case "lost reversal response":
				fake.lostStockReverse = true
			case "lost reversal checkpoint":
				journal.failStockReverseSave = true
			case "lost launch response":
				fake.lostStockLaunch = true
			case "lost launch checkpoint":
				journal.failStockLaunchSave = true
			case "reversal rejected":
				fake.rejectStockReverse = true
			case "launch rejected":
				fake.rejectStockLaunch = true
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := provider.FinalizePaidCheckout(ctx, op, journal); err == nil {
				t.Fatal("expected interrupted operation")
			}
			_, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal)
			uncertain := scenario == "lost launch response" || scenario == "lost launch checkpoint"
			if uncertain {
				var conflict *providers.TinyCheckoutReconciliationError
				if !errors.As(err, &conflict) || journal.bound != "" {
					t.Fatalf("ambiguous launch completed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if fake.stockReversals != 1 || fake.stockLaunches != 1 || fake.posts != 1 || fake.cancels != 1 {
				t.Fatalf("reversals=%d launches=%d creates=%d cancels=%d", fake.stockReversals, fake.stockLaunches, fake.posts, fake.cancels)
			}
		})
	}
}

func TestTinyPaidStockRecoveryPreservesProtectedOrders(t *testing.T) {
	for _, scenario := range []string{"invoice", "receipt", "invoice after replacement", "merchant change after replacement", "creation rejected"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			fake.stockLocked = true
			switch scenario {
			case "invoice":
				fake.orders["1"].InvoiceID = 99
			case "receipt":
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: 49.9, Balance: 49.9}}
				fake.received = true
			case "invoice after replacement":
				fake.invoiceAfterCreate = true
			case "merchant change after replacement":
				fake.merchantChangeAfterCreate = true
			case "creation rejected":
				fake.rejectCreate = true
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := provider.FinalizePaidCheckout(ctx, op, &checkoutTestJournal{}); err == nil {
				t.Fatal("protected order finalized")
			}
			if fake.stockReversals != 0 || fake.stockLaunches != 0 || fake.cancels != 0 {
				t.Fatal("source stock changed before a safe replacement")
			}
		})
	}
}

func TestTinyPaidStockRecoveryKeepsSameOrderWhenCommercialDataMatches(t *testing.T) {
	provider, fake, op := checkoutFinalizationFixture(t)
	fake.stockLocked, fake.noReplacement = true, true
	source := fake.orders["1"]
	source.Freight, source.Discount, source.Total = 18.59, 2.5, 65.99
	source.Payment.Installments = []tinyCheckoutInstallment{{Value: 65.99, Date: "2026-09-14", Note: "old installment"}}
	source.Payment.Installments[0].Method.Name = "Pix"
	journal := &checkoutTestJournal{}
	result, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrderID != "1" || fake.posts != 0 || fake.cancels != 0 || fake.stockReversals != 1 || fake.stockLaunches != 1 {
		t.Fatalf("result=%+v reversals=%d launches=%d creates=%d cancels=%d", result, fake.stockReversals, fake.stockLaunches, fake.posts, fake.cancels)
	}
}

func TestTinyPaidStockRecoveryDoesNotLaunchUnrelatedReservations(t *testing.T) {
	provider, fake, op := checkoutFinalizationFixture(t)
	if _, err := provider.FinalizePaidCheckout(t.Context(), op, &checkoutTestJournal{}); err != nil {
		t.Fatal(err)
	}
	if fake.stockReversals != 0 || fake.stockLaunches != 0 {
		t.Fatal("ordinary reservation received stock movement")
	}
}

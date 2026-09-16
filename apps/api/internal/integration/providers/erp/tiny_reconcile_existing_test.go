package erp

import (
	"errors"
	"fmt"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func consistentFinalizedTinyOrder(t *testing.T) (*Tiny, *checkoutTestTiny, *providers.TinyCheckoutOperation) {
	t.Helper()
	provider, fake, op := checkoutFinalizationFixture(t)
	source := fake.orders["1"]
	source.Status, source.InvoiceID = providers.SituacaoFaturada, 99
	source.Customer.Name = "  COMPRADOR   TESTE "
	source.Total, source.Freight, source.Discount = 65.99, 18.59, 2.50
	payment := op.Order.Checkout.Payments[0]
	parcel := tinyCheckoutInstallment{Value: 65.99, Date: payment.DueDate.Format("2006-01-02")}
	parcel.Method.ID, parcel.Method.Name = 7, "Pix"
	source.Payment.Installments = []tinyCheckoutInstallment{parcel}
	fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "pago", Value: 65.99, Balance: 0, DueDate: parcel.Date}}
	return provider, fake, op
}

func TestTinyReconcilesFinalizedOrderWithoutAnyERPWrite(t *testing.T) {
	for _, status := range []int{1, 4, 7, 5, 6} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			provider, fake, op := consistentFinalizedTinyOrder(t)
			fake.orders["1"].Status = status
			journal := &checkoutTestJournal{}
			result, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if err != nil {
				t.Fatal(err)
			}
			expected, _ := providers.ERPOrderStatusFromSituacao(status)
			if result.OrderID != "1" || !op.Completed || op.TargetStatus != expected || journal.bound != "1" {
				t.Fatalf("incorrect reconciliation: %+v", op)
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
				t.Fatal(err)
			}
			if fake.writes != 0 {
				t.Fatalf("read-only reconciliation sent %d ERP writes", fake.writes)
			}
		})
	}
}

func TestTinyFinalizedReconciliationKeepsRealDifferencesPending(t *testing.T) {
	for _, scenario := range []string{"freight", "items", "customer", "installments", "payment method", "accounts", "missing accounts", "cancelled", "unknown status", "wrong owner", "ambiguous replacement"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := consistentFinalizedTinyOrder(t)
			source := fake.orders["1"]
			switch scenario {
			case "freight":
				source.Freight = 0
			case "items":
				source.Items[0].Quantity = 2
			case "customer":
				source.Customer.Name = "Outro comprador"
			case "installments":
				source.Payment.Installments = nil
			case "payment method":
				source.Payment.Installments[0].Method.Name = "Dinheiro"
			case "accounts":
				fake.accounts["1"][0].Value = 49.90
			case "missing accounts":
				fake.accounts["1"] = nil
			case "cancelled":
				source.Status = providers.SituacaoCancelada
			case "unknown status":
				source.Status = 999
			case "wrong owner":
				source.Anchor = "lc-cart-someone-else"
			case "ambiguous replacement":
				op.CreateStarted = true
			}
			journal := &checkoutTestJournal{}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if err == nil || op.Completed || journal.bound != "" || fake.writes != 0 {
				t.Fatalf("unsafe reconciliation: err=%v bound=%s writes=%d", err, journal.bound, fake.writes)
			}
			var conflict *providers.TinyCheckoutReconciliationError
			if scenario != "wrong owner" && !errors.As(err, &conflict) {
				t.Fatalf("business conflict lost its type: %v", err)
			}
		})
	}
}

func TestTinyReadOnlyReconciliationResumesAfterLocalBindFailure(t *testing.T) {
	provider, fake, op := consistentFinalizedTinyOrder(t)
	journal := &checkoutTestJournal{bindFailures: 1}
	if err := journal.Save(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.FinalizePaidCheckout(t.Context(), op, journal); err == nil {
		t.Fatal("expected bind failure")
	}
	if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 0 || journal.bound != "1" {
		t.Fatal("retry wrote to ERP or lost existing binding")
	}
}

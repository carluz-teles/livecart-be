package erp

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyExistingCheckoutRecognizesCustomerAddressAndCarrierRegistration(t *testing.T) {
	for _, scenario := range []string{"customer address", "explicit address", "different complement", "missing address", "explicit empty address", "explicit partial address", "other carrier", "misleading carrier name", "missing carrier id"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := consistentFinalizedTinyOrder(t)
			source := fake.orders["1"]
			address := &tinyCheckoutAddress{Street: "Rua de Teste", Number: "42", Complement: "Casa", Neighborhood: "Centro", City: "São Paulo", State: "SP", Zip: "01001-000"}
			source.Customer.Address = address
			op.Order.Checkout.Address = &providers.ERPShippingAddress{Street: "Rua de Teste", Number: "42", Complement: "Casa", Neighborhood: "Centro", City: "São Paulo", State: "SP", ZipCode: "01001000"}
			op.Order.Checkout.Shipping = &providers.ERPOrderShipping{Carrier: "Jadlog", Service: "Jadlog", CostCents: 1859}
			fake.shippingForms = []tinyCheckoutReference{{ID: 11, Name: "Jadlog"}, {ID: 22, Name: "Jadlog via Smart Envios"}}
			source.Shipping.Form = &tinyCheckoutReference{ID: 22, Name: "Jadlog via Smart Envios"}
			var expectedField string
			switch scenario {
			case "explicit address":
				source.Address = address
			case "different complement":
				source.Customer.Address.Complement = "Apartamento 9"
				expectedField = "endereço de entrega"
			case "missing address":
				source.Customer.Address = nil
				expectedField = "endereço de entrega"
			case "explicit empty address":
				source.Address = &tinyCheckoutAddress{}
				expectedField = "endereço de entrega"
			case "explicit partial address":
				source.Address = &tinyCheckoutAddress{Street: address.Street}
				expectedField = "endereço de entrega"
			case "other carrier":
				source.Shipping.Form.Name = "Loggi via Smart Envios"
				expectedField = "forma de envio"
			case "misleading carrier name":
				source.Shipping.Form.Name = "Jadlog Expresso via Smart Envios"
				expectedField = "forma de envio"
			case "missing carrier id":
				source.Shipping.Form.ID = 0
				expectedField = "forma de envio"
			}
			journal := &checkoutTestJournal{}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if expectedField == "" {
				if err != nil || journal.bound != "1" || op.TargetStatus != providers.ERPOrderStatusFaturado {
					t.Fatalf("correct existing order not reconciled: %v", err)
				}
			} else {
				var conflict *providers.TinyCheckoutReconciliationError
				if !errors.As(err, &conflict) || !slices.Equal(conflict.Fields, []string{expectedField}) || journal.bound != "" {
					t.Fatalf("unexpected conflict or binding: %v", err)
				}
			}
			if fake.writes != 0 {
				t.Fatalf("reconciliation wrote to Tiny %d times", fake.writes)
			}
			if scenario == "customer address" {
				if source.Address != nil || slices.Contains(tinyCheckoutDifferences(source, *op.Order.Checkout), "endereço de entrega") {
					t.Fatal("effective delivery address must be recognized without mutating the response")
				}
				if tinyCheckoutShippingMatches(source, 11) {
					t.Fatal("replacement verification must still enforce the requested shipping ID")
				}
			}
		})
	}
}

func TestTinyRetryReportsMerchantAdjustmentsWithoutERPWrite(t *testing.T) {
	for _, scenario := range []string{"rounding expense", "merchant item"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			source := fake.orders["1"]
			// Reproduce the commercial shape of the reported open PIX order:
			// 55.90 + 15.16 freight - percentage discount + 0.01 expense.
			source.Total, source.Freight, source.Discount = 68.27, 15.16, 2.795
			source.Items[0].UnitPrice = 55.90
			op.Order.Items[0].UnitPrice = 5590
			op.Order.Checkout.FreightCents, op.Order.Checkout.DiscountCents = 1516, 279
			op.Order.Checkout.Payments[0].AmountCents = 6827
			op.Order.TotalAmount = 6827
			field := "despesas adicionais do pedido"
			if scenario == "rounding expense" {
				source.OtherExpenses = 0.01
			} else {
				source.Items[0].Quantity = 2
				field = "itens ajustados no ERP"
			}
			journal := &checkoutTestJournal{}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			var conflict *providers.TinyCheckoutReconciliationError
			if !errors.As(err, &conflict) || !slices.Equal(conflict.Fields, []string{field}) || conflict.Status != 0 {
				t.Fatalf("merchant adjustment must be a typed business conflict: %v", err)
			}
			if fake.writes != 0 || journal.bound != "" || op.Prepared {
				t.Fatal("merchant adjustment was overwritten or marked reconciled")
			}
		})
	}
}

func TestTinyRetryReportsInstallmentsAndReceivablesTogether(t *testing.T) {
	provider, fake, op := consistentFinalizedTinyOrder(t)
	source := fake.orders["1"]
	source.Total, source.Freight, source.Discount = 271.98, 26.08, 0
	source.Items[0].UnitPrice = 245.90
	op.Order.Items[0].UnitPrice = 24590
	op.Order.Checkout.FreightCents, op.Order.Checkout.DiscountCents = 2608, 0
	op.Order.Checkout.Payments = nil
	for i, date := range []string{"2026-10-12", "2026-11-11", "2026-12-11", "2027-01-10", "2027-02-09"} {
		due, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatal(err)
		}
		amount := int64(5439)
		if i == 4 {
			amount = 5442
		}
		op.Order.Checkout.Payments = append(op.Order.Checkout.Payments, providers.ERPInstallment{AmountCents: amount, DueDate: due, Method: "credit_card"})
	}
	// Anonymous reproduction of the API's distinct card schedule and single
	// receivable. A matching total alone must not hide either discrepancy.
	err := json.Unmarshal([]byte(`{"parcelas":[{"valor":54.38,"data":"2026-10-13","formaRecebimento":{"nome":"Cartão de crédito"}},{"valor":54.40,"data":"2026-11-13","formaRecebimento":{"nome":"Cartão de crédito"}},{"valor":54.40,"data":"2026-12-14","formaRecebimento":{"nome":"Cartão de crédito"}},{"valor":54.40,"data":"2027-01-14","formaRecebimento":{"nome":"Cartão de crédito"}},{"valor":54.40,"data":"2027-02-14","formaRecebimento":{"nome":"Cartão de crédito"}}]}`), &source.Payment)
	if err != nil {
		t.Fatal(err)
	}
	fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: 271.98, Balance: 271.98, DueDate: "2026-09-28"}}
	journal := &checkoutTestJournal{}
	_, err = provider.FinalizePaidCheckout(t.Context(), op, journal)
	var conflict *providers.TinyCheckoutReconciliationError
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Fields, []string{"parcelas e formas de pagamento", "contas a receber"}) {
		t.Fatalf("financial discrepancies were hidden: %v", err)
	}
	if fake.writes != 0 || journal.bound != "" {
		t.Fatal("financial discrepancy was changed or marked reconciled")
	}
}

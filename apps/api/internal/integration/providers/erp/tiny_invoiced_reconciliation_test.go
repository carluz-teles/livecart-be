package erp

import (
	"errors"
	"slices"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyInvoicedManualFinancialSchedule(t *testing.T) {
	for _, name := range []string{"consolidated", "received", "partial", "split accounts", "local bind retry", "delivery reference"} {
		t.Run(name, func(t *testing.T) {
			provider, fake, op := manualInvoicedTinyFixture(t)
			journal := &checkoutTestJournal{}
			switch name {
			case "received":
				fake.accounts["1"][0].Status, fake.accounts["1"][0].Balance = "pago", 0
			case "partial":
				fake.accounts["1"][0].Status, fake.accounts["1"][0].Balance = "parcial", 100
			case "split accounts":
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: 100, Balance: 100, DueDate: "2026-09-28"}, {ID: 51, Status: "aberto", Value: 171.98, Balance: 171.98, DueDate: "2026-10-28"}}
			case "local bind retry":
				journal.bindFailures = 1
			case "delivery reference":
				op.Order.Checkout.Address = &providers.ERPShippingAddress{Street: "Rua Teste", Number: "42", Complement: "17o andar", Neighborhood: "Centro", City: "São Paulo", State: "SP", ZipCode: "01001000"}
				fake.orders["1"].Customer.Address = &tinyCheckoutAddress{Street: "Rua Teste", Number: "42", Complement: "17o andar ( empresa Teste) ", Neighborhood: "Centro", City: "São Paulo", State: "SP", Zip: "01001-000"}
			}
			if err := journal.Save(t.Context(), op); err != nil {
				t.Fatal(err)
			}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if name == "local bind retry" {
				if err == nil || op.Completed {
					t.Fatal("local binding failure was hidden")
				}
				op = journal.resume(t)
				_, err = provider.FinalizePaidCheckout(t.Context(), op, journal)
			}
			if err != nil || !op.Completed || journal.bound != "1" || op.TargetStatus != providers.ERPOrderStatusFaturado {
				t.Fatalf("manual invoiced order not reconciled: %v", err)
			}
			if !journal.resume(t).PreservedFinancialSchedule || (name == "delivery reference") != op.PreservedDeliveryReference {
				t.Fatal("preservation decision not recorded in the journal")
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
				t.Fatal(err)
			}
			if fake.writes != 0 {
				t.Fatal("invoiced order, contact, stock or accounts were modified")
			}
		})
	}
}

func TestTinyInvoicedDeliveryReferencePreservesDestinationChecks(t *testing.T) {
	for _, name := range []string{"company reference", "labeled reference", "different floor", "different unit in parentheses", "missing complement", "different street", "different number", "different postcode", "not invoiced", "explicit conflicting address"} {
		t.Run(name, func(t *testing.T) {
			_, fake, op := manualInvoicedTinyFixture(t)
			source := fake.orders["1"]
			op.Order.Checkout.Address = &providers.ERPShippingAddress{Street: "Rua Teste", Number: "42", Complement: "17o andar", Neighborhood: "Centro", City: "São Paulo", State: "SP", ZipCode: "01001000"}
			address := &tinyCheckoutAddress{Street: "Rua Teste", Number: "42", Complement: "17o andar ( empresa Teste) ", Neighborhood: "Centro", City: "São Paulo", State: "SP", Zip: "01001-000"}
			source.Customer.Address = address
			switch name {
			case "labeled reference":
				address.Complement = "17o andar (Referência: recepção)"
			case "different floor":
				address.Complement = "18o andar (empresa Teste)"
			case "different unit in parentheses":
				address.Complement = "17o andar (apto 18)"
			case "missing complement":
				op.Order.Checkout.Address.Complement = ""
			case "different street":
				address.Street = "Outra rua"
			case "different number":
				address.Number = "43"
			case "different postcode":
				address.Zip = "01002-000"
			case "not invoiced":
				source.InvoiceID = 0
			case "explicit conflicting address":
				source.Address = &tinyCheckoutAddress{Street: "Outro destino"}
			}
			originalComplement := address.Complement
			differences := existingTinyCheckoutDifferences(source, op, 0)
			accepted := name == "company reference" || name == "labeled reference"
			if slices.Contains(differences, "endereço de entrega") == accepted {
				t.Fatalf("unexpected address decision: %v", differences)
			}
			if !slices.Contains(tinyCheckoutDifferences(source, *op.Order.Checkout), "endereço de entrega") || address.Complement != originalComplement {
				t.Fatal("strict write-path comparison or original response was modified")
			}
		})
	}
}

func TestTinyInvoicedMixedPaymentsKeepAmountsAndCountsByMethod(t *testing.T) {
	for _, name := range []string{"valid", "method count changed", "value moved between methods", "ambiguous method", "missing method"} {
		t.Run(name, func(t *testing.T) {
			provider, fake, op := manualInvoicedTinyFixture(t)
			source := fake.orders["1"]
			op.Order.Checkout.Payments[0].Method = "pix"
			source.Payment.Installments[0].Method.Name = "Pix"
			source.Payment.Installments[0].Value, source.Payment.Installments[1].Value = 54.39, 54.39
			switch name {
			case "method count changed":
				source.Payment.Installments[1].Method.Name = "Pix"
			case "value moved between methods":
				source.Payment.Installments[0].Value -= 0.01
				source.Payment.Installments[1].Value += 0.01
			case "ambiguous method":
				source.Payment.Installments[0].Method.Name = "Pix ou Cartão de Crédito"
			case "missing method":
				op.Order.Checkout.Payments[0].Method = ""
				source.Payment.Installments[0].Method.Name = ""
			}
			journal := &checkoutTestJournal{}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if (err == nil) != (name == "valid") || fake.writes != 0 || op.Completed != (name == "valid") {
				t.Fatalf("mixed payment verification failed: %v", err)
			}
		})
	}
}

func TestTinyInvoicedManualScheduleRejectsRealDifferences(t *testing.T) {
	for _, name := range []string{"no invoice", "cancelled", "incomplete", "unknown status", "freight", "total", "customer", "document", "items", "installment count", "installment method", "ambiguous installment method", "installment total", "invalid installment date", "zero installment", "missing accounts", "account total", "duplicate account", "cancelled account", "unknown account status", "negative balance", "excess balance", "invalid account date", "accounts changed during verification"} {
		t.Run(name, func(t *testing.T) {
			provider, fake, op := manualInvoicedTinyFixture(t)
			source := fake.orders["1"]
			switch name {
			case "no invoice":
				source.InvoiceID = 0
			case "cancelled":
				source.Status = providers.SituacaoCancelada
			case "incomplete":
				source.Status = providers.SituacaoDadosIncompletos
			case "unknown status":
				source.Status = 999
			case "freight":
				source.Freight = 0
			case "total":
				source.Total++
			case "customer":
				source.Customer.Name = "Outro comprador"
			case "document":
				op.Order.Checkout.Customer.CpfCnpj = "52998224725"
				source.Customer.Document = "11144477735"
			case "items":
				source.Items[0].Quantity++
			case "installment count":
				source.Payment.Installments[0].Value += source.Payment.Installments[4].Value
				source.Payment.Installments = source.Payment.Installments[:4]
			case "installment method":
				source.Payment.Installments[0].Method.Name = "Pix"
			case "ambiguous installment method":
				source.Payment.Installments[0].Method.Name = "Pix ou Cartão de crédito"
			case "installment total":
				source.Payment.Installments[0].Value += 0.01
			case "invalid installment date":
				source.Payment.Installments[0].Date = "2026-02-30"
			case "zero installment":
				source.Payment.Installments[1].Value += source.Payment.Installments[0].Value
				source.Payment.Installments[0].Value = 0
			case "missing accounts":
				fake.accounts["1"] = nil
			case "account total":
				fake.accounts["1"][0].Value -= 0.01
			case "duplicate account":
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: 135.99, Balance: 135.99, DueDate: "2026-09-28"}, {ID: 50, Status: "aberto", Value: 135.99, Balance: 135.99, DueDate: "2026-09-28"}}
			case "cancelled account":
				fake.accounts["1"][0].Status = "cancelada"
			case "unknown account status":
				fake.accounts["1"][0].Status = "desconhecido"
			case "negative balance":
				fake.accounts["1"][0].Balance = -1
			case "excess balance":
				fake.accounts["1"][0].Balance = 300
			case "invalid account date":
				fake.accounts["1"][0].DueDate = "2026-02-30"
			case "accounts changed during verification":
				fake.changeAccountsOnReread = true
			}
			journal := &checkoutTestJournal{}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			var conflict *providers.TinyCheckoutReconciliationError
			if !errors.As(err, &conflict) || op.Completed || journal.bound != "" || fake.writes != 0 {
				t.Fatalf("unsafe reconciliation: error=%v bound=%s writes=%d", err, journal.bound, fake.writes)
			}
		})
	}
}

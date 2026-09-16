package erp

import (
	"math"
	"strings"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// Only an existing invoiced sale can retain a merchant's financial calendar.
// Reservation creation, replacement verification and installment writes remain strict.
func tinyPreservesInvoicedSchedule(order *tinyCheckoutOrder) bool {
	if order.InvoiceID <= 0 {
		return false
	}
	switch order.Status {
	case providers.SituacaoFaturada, providers.SituacaoPreparandoEnvio, providers.SituacaoProntoEnvio,
		providers.SituacaoEnviada, providers.SituacaoEntregue, providers.SituacaoNaoEntregue:
		return true
	}
	return false
}

func tinyInvoicedInstallmentsMatch(current []tinyCheckoutInstallment, desired []providers.ERPInstallment) bool {
	if len(current) == 0 || len(current) != len(desired) {
		return false
	}
	type paymentGroup struct {
		count int
		cents int64
	}
	remaining := map[string]paymentGroup{}
	for _, payment := range desired {
		group := remaining[payment.Method]
		if len(formaRecebimentoAliases(payment.Method)) == 0 || payment.AmountCents <= 0 || payment.AmountCents > math.MaxInt64-group.cents {
			return false
		}
		group.count++
		group.cents += payment.AmountCents
		remaining[payment.Method] = group
	}
	for _, payment := range current {
		method := ""
		for _, candidate := range []string{"pix", "credit_card", "debit_card", "boleto"} {
			if matchesFormaRecebimento(candidate, payment.Method.Name) {
				if method != "" { // Ambiguous ERP labels must not move value between methods.
					return false
				}
				method = candidate
			}
		}
		group, found := remaining[method]
		if !found || group.count <= 0 || !tinyFinancialDateValid(payment.Date) {
			return false
		}
		cents, valid := tinyPositiveFinancialCents(payment.Value, group.cents)
		if !valid {
			return false
		}
		group.count--
		group.cents -= cents
		remaining[method] = group
	}
	for _, group := range remaining {
		if group.count != 0 || group.cents != 0 {
			return false
		}
	}
	return true
}

func tinyInvoicedReceivablesMatch(accounts []tinyReceivable, desired []providers.ERPInstallment) bool {
	// Consolidation can reduce title count; missing, duplicated or extra value
	// must never be interpreted as successful financial synchronization.
	if len(accounts) == 0 || len(accounts) > len(desired) {
		return false
	}
	var remaining int64
	for _, payment := range desired {
		if payment.AmountCents <= 0 || payment.AmountCents > math.MaxInt64-remaining {
			return false
		}
		remaining += payment.AmountCents
	}
	seen := map[int64]bool{}
	for _, account := range accounts {
		if account.ID <= 0 || seen[account.ID] || !tinyFinancialDateValid(account.DueDate) {
			return false
		}
		seen[account.ID] = true
		switch account.Status {
		case "aberto", "atrasadas", "pago", "parcial":
		default:
			return false
		}
		cents, valid := tinyPositiveFinancialCents(account.Value, remaining)
		if !valid || math.IsNaN(account.Balance) || account.Balance < 0 || account.Balance > account.Value {
			return false
		}
		remaining -= cents
	}
	return remaining == 0
}

func tinyPositiveFinancialCents(value float64, maximum int64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, false
	}
	cents := math.Round(value * 100)
	if cents <= 0 || cents > float64(maximum) {
		return 0, false
	}
	return int64(cents), true
}

func tinyFinancialDateValid(value string) bool {
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04:05", time.RFC3339} {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}

// Recognize an appended, explicitly labeled delivery reference without
// ignoring the original complement or accepting a different apartment/floor.
func tinyDeliveryReferenceAdded(actual, expected string) bool {
	normalize := func(value string) string {
		return strings.ToLower(strings.Join(strings.Fields(value), " "))
	}
	wanted := normalize(expected)
	if wanted == "" {
		return false
	}
	suffix, added := strings.CutPrefix(normalize(actual), wanted+" (")
	if !added || !strings.HasSuffix(suffix, ")") {
		return false
	}
	reference := strings.TrimSpace(strings.TrimSuffix(suffix, ")"))
	if strings.ContainsAny(reference, "()") {
		return false
	}
	for _, label := range []string{"empresa ", "empresa:", "referência:", "referencia:"} {
		if text, found := strings.CutPrefix(reference, label); found && strings.TrimSpace(text) != "" {
			return true
		}
	}
	return false
}

func tinyInvoicedCommercialSnapshot(source *tinyCheckoutOrder, checkout providers.ERPOrderCheckout) tinyCheckoutOrder {
	commercial := *source
	if !tinyPreservesInvoicedSchedule(source) || checkout.Address == nil {
		return commercial
	}
	address := source.Address
	if address == nil {
		address = source.Customer.Address
	}
	if address != nil && tinyDeliveryReferenceAdded(address.Complement, checkout.Address.Complement) {
		copyAddress := *address
		copyAddress.Complement = checkout.Address.Complement
		commercial.Address = &copyAddress
	}
	return commercial
}

package erp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"livecart/apps/api/internal/integration/providers"
)

type tinyRecordedReceipt struct {
	ID       int64    `json:"id"`
	Date     string   `json:"data"`
	Type     int      `json:"tipo"`
	Paid     *float64 `json:"valorPago"`
	Fee      *float64 `json:"valorTaxa"`
	Interest *float64 `json:"valorJuro"`
	Discount *float64 `json:"valorDesconto"`
	Addition *float64 `json:"valorAcrescimo"`
}

// VerifyRecordedOrderPayment reconciles money acknowledged by the merchant in
// Tiny. It only reads: neither approval nor invoice proves that money settled.
// The merchant's titles may consolidate card installments or combine methods.
func (t *Tiny) VerifyRecordedOrderPayment(ctx context.Context, orderID string, paidCents int64) error {
	id, err := strconv.ParseInt(orderID, 10, 64)
	if err != nil || id <= 0 || paidCents <= 0 {
		return fmt.Errorf("tiny: invalid external payment reference")
	}
	order, err := t.readCheckoutOrder(ctx, orderID)
	if err != nil {
		return err
	}
	switch order.Status {
	case providers.SituacaoAprovada, providers.SituacaoFaturada, providers.SituacaoPreparandoEnvio,
		providers.SituacaoProntoEnvio, providers.SituacaoEnviada, providers.SituacaoEntregue:
	default:
		return fmt.Errorf("tiny: situação do pedido não comprova venda aprovada")
	}
	total, valid := tinyPositiveFinancialCents(order.Total, paidCents)
	if order.ID != id || !valid || total != paidCents {
		return fmt.Errorf("tiny: total do pedido diverge do pagamento externo registrado")
	}
	accounts, err := t.checkoutReceivables(ctx, orderID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("tiny: pagamento externo sem títulos verificáveis")
	}
	remaining := paidCents
	seenAccounts := map[int64]bool{}
	seenReceipts := map[int64]bool{}
	for _, account := range accounts {
		value, valid := tinyPositiveFinancialCents(account.Value, remaining)
		identified := account.ID > 0 && !seenAccounts[account.ID] && tinyFinancialDateValid(account.DueDate)
		settled := account.Status == "pago" && account.Balance == 0
		if !identified || !valid || !settled {
			return fmt.Errorf("tiny: título externo sem quitação verificável")
		}
		seenAccounts[account.ID] = true
		var receipts []tinyRecordedReceipt
		path := fmt.Sprintf("/contas-receber/%d/recebimentos", account.ID)
		if err := t.checkoutRequest(ctx, http.MethodGet, path, nil, &receipts); err != nil {
			return err
		}
		if !tinyRecordedReceiptsCover(receipts, value, seenReceipts) {
			return fmt.Errorf("tiny: recebimentos e taxas não comprovam quitação do título externo")
		}
		remaining -= value
	}
	if remaining != 0 {
		return fmt.Errorf("tiny: títulos externos não cobrem o pagamento registrado")
	}
	return nil
}

func tinyRecordedReceiptsCover(receipts []tinyRecordedReceipt, remaining int64, seen map[int64]bool) bool {
	if len(receipts) == 0 {
		return false
	}
	for _, receipt := range receipts {
		identified := receipt.ID > 0 && !seen[receipt.ID] && tinyFinancialDateValid(receipt.Date)
		// Type 1 is the receipt shape observed in settled Tiny titles. Reversals
		// or unknown movements and adjustments need separate reconciliation.
		if !identified || receipt.Type != 1 {
			return false
		}
		seen[receipt.ID] = true
		for _, adjustment := range []*float64{receipt.Interest, receipt.Discount, receipt.Addition} {
			if adjustment == nil || *adjustment != 0 {
				return false
			}
		}
		if receipt.Paid == nil || receipt.Fee == nil {
			return false
		}
		net, valid := tinyPositiveFinancialCents(*receipt.Paid, remaining)
		if !valid {
			return false
		}
		remaining -= net
		if *receipt.Fee != 0 {
			fee, valid := tinyPositiveFinancialCents(*receipt.Fee, remaining)
			if !valid {
				return false
			}
			remaining -= fee
		}
	}
	return remaining == 0
}

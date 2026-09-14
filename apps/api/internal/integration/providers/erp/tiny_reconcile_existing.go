package erp

import (
	"context"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

// Reconcile an already finalized sale using GETs only. An invoice forbids
// rewriting the ERP, but does not imply that an otherwise correct sale failed.
func (t *Tiny) reconcileExistingCheckout(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal, source *tinyCheckoutOrder) (*OrderResult, error) {
	conflict := &providers.TinyCheckoutReconciliationError{OrderID: op.SourceID, Status: source.Status, InvoiceID: source.InvoiceID}
	status, known := providers.ERPOrderStatusFromSituacao(source.Status)
	if !known || status == providers.ERPOrderStatusCancelado || status == providers.ERPOrderStatusDadosIncompletos {
		return nil, conflict
	}
	checkout := *op.Order.Checkout
	var shippingID int64
	if shipping := checkout.Shipping; shipping != nil && !isStorePickup(shipping.Carrier) {
		var err error
		shippingID, _, err = t.resolveShippingForm(ctx, shipping.Carrier)
		if err != nil {
			return nil, err
		}
		if shippingID == 0 {
			conflict.Fields = []string{"forma de envio"}
			return nil, conflict
		}
	}
	accounts, err := t.checkoutReceivables(ctx, op.SourceID)
	if err != nil {
		return nil, err
	}
	return t.completeExistingCheckout(ctx, op, journal, source, shippingID, accounts)
}

func tinyCheckoutCanReuseSource(op *providers.TinyCheckoutOperation) bool {
	return !op.CreateStarted && !op.SourceCancelled && !op.AccountsCleared &&
		(op.TargetID == "" || op.TargetID == op.SourceID)
}

func (t *Tiny) completeExistingCheckout(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal, source *tinyCheckoutOrder, shippingID int64, accounts []tinyReceivable) (*OrderResult, error) {
	conflict := &providers.TinyCheckoutReconciliationError{OrderID: op.SourceID, Status: source.Status, InvoiceID: source.InvoiceID}
	checkout := *op.Order.Checkout
	conflict.Fields = existingTinyCheckoutDifferences(source, op, shippingID)
	// Received titles are valid evidence: never reverse them. Missing or
	// divergent titles cannot prove the financial synchronization succeeded.
	if !tinyExistingCheckoutReceivablesMatch(accounts, checkout.Payments) {
		conflict.Fields = append(conflict.Fields, "contas a receber")
	}
	if len(conflict.Fields) > 0 {
		return nil, conflict
	}
	verified, err := t.readCheckoutOrder(ctx, op.SourceID)
	if err != nil {
		return nil, err
	}
	if verified.Anchor != source.Anchor || verified.Status != source.Status || verified.InvoiceID != source.InvoiceID || len(existingTinyCheckoutDifferences(verified, op, shippingID)) > 0 {
		conflict.Fields = []string{"pedido alterado durante a conferência"}
		return nil, conflict
	}
	approvedExisting := false
	if verified.Status == 0 && verified.InvoiceID == 0 {
		// The sale is already complete; approving it does not require changing
		// its items, stock, contact, installments or received titles.
		if err := t.SetOrderSituacao(ctx, op.SourceID, providers.SituacaoAprovada); err != nil {
			return nil, err
		}
		approvedExisting = true
		verified, err = t.readCheckoutOrder(ctx, op.SourceID)
		if err != nil {
			return nil, err
		}
		if verified.Anchor != source.Anchor || verified.Status != providers.SituacaoAprovada || verified.InvoiceID != source.InvoiceID || len(existingTinyCheckoutDifferences(verified, op, shippingID)) > 0 {
			conflict.Fields = []string{"aprovação ou dados do pedido alterados durante a conferência"}
			return nil, conflict
		}
		accounts, err = t.checkoutReceivables(ctx, op.SourceID)
		if err != nil {
			return nil, err
		}
		if !tinyExistingCheckoutReceivablesMatch(accounts, checkout.Payments) {
			conflict.Fields = []string{"contas a receber alteradas durante a conferência"}
			return nil, conflict
		}
	}
	status, known := providers.ERPOrderStatusFromSituacao(verified.Status)
	if !known || status == providers.ERPOrderStatusCancelado || status == providers.ERPOrderStatusDadosIncompletos {
		return nil, conflict
	}
	op.TargetID, op.TargetNumber, op.TargetStatus = op.SourceID, verified.Number.String(), status
	op.Replace = false
	if err := journal.Bind(ctx, op); err != nil {
		return nil, err
	}
	t.Logger.Info("tiny existing paid checkout reconciled",
		zap.String("cart_id", op.CartID), zap.String("operation_id", op.ID),
		zap.String("order_id", op.SourceID), zap.String("erp_status", string(status)),
		zap.Bool("has_invoice", verified.InvoiceID != 0),
		zap.Bool("approved_existing_order", approvedExisting),
		zap.Bool("receivable_dates_preserved", !tinyReceivablesMatch(accounts, checkout.Payments)),
		zap.Bool("rounding_adjustment_preserved", tinyCheckoutHasCompensatedRounding(source, checkout)))
	return &OrderResult{OrderID: op.TargetID, OrderNumber: op.TargetNumber}, nil
}

// A PIX installment records the captured payment day, whereas a receivable
// may use the merchant's financial due date. On an otherwise verified sale,
// preserve that date without moving the title or posting a receipt. Card,
// mixed or split schedules retain the strict comparison.
func tinyExistingCheckoutReceivablesMatch(accounts []tinyReceivable, desired []providers.ERPInstallment) bool {
	if tinyReceivablesMatch(accounts, desired) {
		return true
	}
	if len(accounts) != 1 || len(desired) != 1 || desired[0].Method != "pix" {
		return false
	}
	account := accounts[0]
	if account.Status != "aberto" && account.Status != "atrasadas" && account.Status != "pago" && account.Status != "parcial" {
		return false
	}
	if account.Balance < 0 || account.Balance > account.Value || len(account.DueDate) < 10 {
		return false
	}
	due, err := time.Parse("2006-01-02", account.DueDate[:10])
	if err != nil {
		return false
	}
	expected := desired[0]
	expected.DueDate = due
	return tinyReceivablesMatch(accounts, []providers.ERPInstallment{expected})
}

func existingTinyCheckoutDifferences(source *tinyCheckoutOrder, op *providers.TinyCheckoutOperation, shippingID int64) []string {
	checkout := *op.Order.Checkout
	// A separate delivery address is optional in Tiny. Use the customer's
	// address only when it is absent, never to hide an explicit partial or
	// different delivery address. Keep this fallback out of the write path.
	commercial := *source
	if commercial.Address == nil {
		commercial.Address = source.Customer.Address
	}
	compensatedRounding := tinyCheckoutHasCompensatedRounding(source, checkout)
	if compensatedRounding {
		commercial.Discount = float64(checkout.DiscountCents) / 100
	}
	fields := tinyCheckoutDifferences(&commercial, checkout)
	if source.OtherExpenses != 0 && !compensatedRounding {
		fields = append(fields, "despesas adicionais do pedido")
	}
	if !tinyCheckoutGridMatches(source, op.Order.Items) {
		fields = append(fields, "itens")
	}
	// Merchant-entered payment notes may differ; amount, due date and method
	// must still agree on each installment, including duplicate amounts.
	if !tinyInstallmentsMatchWith(source.Payment.Installments, checkout.Payments, func(existing tinyCheckoutInstallment, wanted providers.ERPInstallment) bool {
		return matchesFormaRecebimento(wanted.Method, existing.Method.Name)
	}) {
		fields = append(fields, "parcelas e formas de pagamento")
	}
	if checkout.Shipping != nil && !tinyExistingCheckoutShippingMatches(source, checkout.Shipping.Carrier, shippingID) {
		fields = append(fields, "forma de envio")
	}
	return fields
}

// Tiny can retain a percentage discount with fractions of a cent and a one-cent
// expense that compensates its rounding. Accept only that exact compensation
// on an existing order; neither its paid total nor its ERP values may change.
func tinyCheckoutHasCompensatedRounding(source *tinyCheckoutOrder, checkout providers.ERPOrderCheckout) bool {
	if math.Abs(source.OtherExpenses-0.01) > 0.0000001 ||
		int64(math.Round(source.Discount*100)) != checkout.DiscountCents+1 ||
		int64(math.Round((source.Discount-source.OtherExpenses)*100)) != checkout.DiscountCents {
		return false
	}
	var paid int64
	for _, payment := range checkout.Payments {
		paid += payment.AmountCents
	}
	return len(checkout.Payments) > 0 && int64(math.Round(source.Total*100)) == paid
}

func tinyExistingCheckoutShippingMatches(order *tinyCheckoutOrder, carrier string, expectedID int64) bool {
	if tinyCheckoutShippingMatches(order, expectedID) {
		return true
	}
	if order.Shipping.Form == nil || order.Shipping.Form.ID <= 0 || strings.TrimSpace(carrier) == "" || isStorePickup(carrier) {
		return false
	}
	// Tiny may contain a direct carrier and a Smart Envios registration for
	// that same carrier. An already issued sale can keep either registration;
	// creating a replacement still verifies the exact ID it requested.
	name := order.Shipping.Form.Name
	return sameTinyCheckoutText(name, carrier) ||
		sameTinyCheckoutText(name, strings.TrimSpace(carrier)+" via Smart Envios") ||
		sameTinyCheckoutText(name, strings.TrimSpace(carrier)+" via SmartEnvios")
}

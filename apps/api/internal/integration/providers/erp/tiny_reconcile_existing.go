package erp

import (
	"context"

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
	conflict.Fields = existingTinyCheckoutDifferences(source, op, shippingID)
	if len(conflict.Fields) > 0 {
		return nil, conflict
	}
	accounts, err := t.checkoutReceivables(ctx, op.SourceID)
	if err != nil {
		return nil, err
	}
	// Received titles are valid evidence: never reverse them. Missing or
	// divergent titles cannot prove the financial synchronization succeeded.
	if !tinyReceivablesMatch(accounts, checkout.Payments) {
		conflict.Fields = []string{"contas a receber"}
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
	op.TargetID, op.TargetNumber, op.TargetStatus = op.SourceID, verified.Number.String(), status
	if err := journal.Bind(ctx, op); err != nil {
		return nil, err
	}
	t.Logger.Info("tiny existing paid checkout reconciled without ERP writes",
		zap.String("cart_id", op.CartID), zap.String("operation_id", op.ID),
		zap.String("order_id", op.SourceID), zap.String("erp_status", string(status)),
		zap.Bool("has_invoice", verified.InvoiceID != 0))
	return &OrderResult{OrderID: op.TargetID, OrderNumber: op.TargetNumber}, nil
}

func existingTinyCheckoutDifferences(source *tinyCheckoutOrder, op *providers.TinyCheckoutOperation, shippingID int64) []string {
	checkout := *op.Order.Checkout
	fields := tinyCheckoutDifferences(source, checkout)
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
	if checkout.Shipping != nil && !tinyCheckoutShippingMatches(source, shippingID) {
		fields = append(fields, "forma de envio")
	}
	return fields
}

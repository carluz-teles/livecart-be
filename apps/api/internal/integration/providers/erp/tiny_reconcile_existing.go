package erp

import (
	"context"
	"strings"

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
	accounts, err := t.checkoutReceivables(ctx, op.SourceID)
	if err != nil {
		return nil, err
	}
	// Received titles are valid evidence: never reverse them. Missing or
	// divergent titles cannot prove the financial synchronization succeeded.
	if !tinyReceivablesMatch(accounts, checkout.Payments) {
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
	// A separate delivery address is optional in Tiny. Use the customer's
	// address only when it is absent, never to hide an explicit partial or
	// different delivery address. Keep this fallback out of the write path.
	commercial := *source
	if commercial.Address == nil {
		commercial.Address = source.Customer.Address
	}
	fields := tinyCheckoutDifferences(&commercial, checkout)
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

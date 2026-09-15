package erp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

// The caller holds the distributed cart claim and ERP write queue throughout
// finalization. Tiny stock endpoints are not safe to invoke concurrently.
type tinyStockRequestError struct {
	err        error
	notApplied bool
}

func (e *tinyStockRequestError) Error() string { return e.err.Error() }
func (e *tinyStockRequestError) Unwrap() error { return e.err }

func (t *Tiny) checkoutStockRequest(ctx context.Context, orderID, action string) error {
	endpoint := tinyAPIBaseURL + "/pedidos/" + orderID + "/" + action
	resp, body, err := t.DoRequestRetrying429(ctx, 2, http.MethodPost, endpoint, nil, t.authHeaders())
	if err != nil {
		var notSent *providers.RequestNotSentError
		return &tinyStockRequestError{err: fmt.Errorf("tiny %s: %w", action, err),
			notApplied: errors.As(err, &notSent) || (resp != nil && resp.StatusCode == http.StatusTooManyRequests)}
	}
	if providers.IsSuccessStatus(resp.StatusCode) {
		return nil
	}
	// Exact rejection observed in the Tiny sandbox. A generic 400 or a stock
	// validation error must never be mistaken for proof of a completed launch.
	var rejection struct {
		Message string `json:"mensagem"`
	}
	if action == "lancar-estoque" && resp.StatusCode == http.StatusBadRequest &&
		json.Unmarshal(body, &rejection) == nil &&
		strings.EqualFold(strings.TrimSpace(rejection.Message), "Estoque já lançado.") {
		return nil
	}
	status := resp.StatusCode
	notApplied := status == 400 || status == 401 || status == 403 || status == 404 || status == 422 || status == 429
	return &tinyStockRequestError{err: fmt.Errorf("tiny %s: status %d: %s", action, status, tinyErrorDetail(body)), notApplied: notApplied}
}

func saveStockCheckpoint(ctx context.Context, journal providers.TinyCheckoutJournal, op *providers.TinyCheckoutOperation) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return journal.Save(cleanup, op)
}

// A stock lock may be handled later, once a verified replacement holds the
// reservation. Checking it must not release the source prematurely.
func (t *Tiny) checkCheckoutSourceLock(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal, source *tinyCheckoutOrder, allowAccounts bool) error {
	err := t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source))
	if errors.Is(err, providers.ErrOrderStockLaunched) {
		if op.StockReversed {
			return &providers.TinyCheckoutReconciliationError{OrderID: op.SourceID, Fields: []string{"estoque relançado na origem durante a finalização"}}
		}
		if !op.SourceStockLaunched {
			op.SourceStockLaunched = true
			if err := journal.Save(ctx, op); err != nil {
				return err
			}
			t.Logger.Info("tiny paid checkout stock lock detected", zap.String("cart_id", op.CartID),
				zap.String("operation_id", op.ID), zap.String("source_order_id", op.SourceID))
		}
		return nil
	}
	if allowAccounts && errors.Is(err, providers.ErrOrderAccountsLaunched) {
		return nil
	}
	return err
}

func (t *Tiny) unlockPaidCheckoutStock(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal, source *tinyCheckoutOrder) error {
	if op.Replace && (source.OtherExpenses != 0 || !tinyCheckoutPreservesMerchantItems(source, op.Order.Items)) {
		return &providers.TinyCheckoutReconciliationError{OrderID: op.SourceID, Fields: []string{"itens ou despesas alterados na origem durante a finalização"}}
	}
	err := t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source))
	if err == nil {
		// An interrupted reversal may have succeeded. The previously recorded
		// stock lock plus a now-editable source proves it is no longer launched.
		if op.StockReverseStarted && !op.StockReversed {
			op.StockReversed = true
			return saveStockCheckpoint(ctx, journal, op)
		}
		return nil
	}
	if !errors.Is(err, providers.ErrOrderStockLaunched) {
		return err
	}
	if op.StockReverseStarted || op.StockReversed {
		return &providers.TinyCheckoutReconciliationError{OrderID: op.SourceID, Fields: []string{"resultado do estorno de estoque; operação não repetida"}}
	}
	// Re-read fiscal and financial state immediately before the reversal.
	fresh, err := t.sourceForCheckout(ctx, op)
	if err != nil {
		return err
	}
	if fresh.OtherExpenses != source.OtherExpenses || !tinyCheckoutGridMatches(fresh, tinyCheckoutGrid(source)) {
		return &providers.TinyCheckoutReconciliationError{OrderID: op.SourceID, Fields: []string{"itens ou despesas alterados antes do estorno de estoque"}}
	}
	source = fresh
	accounts, err := t.checkoutReceivables(ctx, op.SourceID)
	if err != nil {
		return err
	}
	if len(accounts) != 0 {
		return fmt.Errorf("tiny: contas lançadas durante a finalização; estorno de estoque adiado")
	}
	op.SourceStockLaunched, op.StockReverseStarted = true, true
	if err := journal.Save(ctx, op); err != nil {
		return err
	}
	err = t.ReverseOrderStock(ctx, op.SourceID)
	if err != nil {
		var rejected *tinyStockRequestError
		if errors.As(err, &rejected) && rejected.notApplied {
			op.StockReverseStarted = false
			return errors.Join(err, saveStockCheckpoint(ctx, journal, op))
		}
		return fmt.Errorf("tiny: resultado do estorno de estoque não confirmado: %w", err)
	}
	op.StockReversed = true
	if err := saveStockCheckpoint(ctx, journal, op); err != nil {
		return err
	}
	t.Logger.Info("tiny paid checkout stock reversed", zap.String("cart_id", op.CartID),
		zap.String("operation_id", op.ID), zap.String("source_order_id", op.SourceID), zap.String("target_order_id", op.TargetID))
	// Prove the lock was removed; an HTTP 204 alone cannot authorize cancelling
	// a source whose inventory is still launched.
	return t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source))
}

func (t *Tiny) restorePaidCheckoutStock(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal) error {
	if !op.StockReversed || op.StockLaunched {
		return nil
	}
	if op.StockLaunchStarted {
		return &providers.TinyCheckoutReconciliationError{OrderID: op.TargetID, Fields: []string{"resultado do relançamento de estoque; operação não repetida"}}
	}
	target, err := t.readCheckoutOrder(ctx, op.TargetID)
	if err != nil {
		return err
	}
	if target.InvoiceID != 0 || target.Status != providers.SituacaoAprovada {
		return &providers.TinyCheckoutReconciliationError{OrderID: op.TargetID, Status: target.Status, InvoiceID: target.InvoiceID}
	}
	anchor := op.SourceAnchor
	if anchor == "" {
		anchor = tinyCartMarker(op.CartID)
	}
	if op.Replace {
		anchor = tinyCartMarker(op.Order.ExternalID)
	}
	if target.Anchor != anchor || len(tinyCheckoutDifferences(target, *op.Order.Checkout)) != 0 ||
		!tinyCheckoutGridMatches(target, op.Order.Items) || !tinyInstallmentsMatch(target.Payment.Installments, op.Order.Checkout.Payments) {
		return fmt.Errorf("tiny: pedido final alterado antes do relançamento de estoque")
	}
	op.StockLaunchStarted = true
	if err := journal.Save(ctx, op); err != nil {
		return err
	}
	err = t.checkoutStockRequest(ctx, op.TargetID, "lancar-estoque")
	if err != nil {
		var rejected *tinyStockRequestError
		if errors.As(err, &rejected) && rejected.notApplied {
			op.StockLaunchStarted = false
			return errors.Join(err, saveStockCheckpoint(ctx, journal, op))
		}
		return fmt.Errorf("tiny: resultado do relançamento de estoque não confirmado: %w", err)
	}
	op.StockLaunched = true
	if err := saveStockCheckpoint(ctx, journal, op); err != nil {
		return err
	}
	t.Logger.Info("tiny paid checkout stock restored", zap.String("cart_id", op.CartID),
		zap.String("operation_id", op.ID), zap.String("target_order_id", op.TargetID))
	return nil
}

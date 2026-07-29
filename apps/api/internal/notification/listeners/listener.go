// Package listeners contains the Notification module's event reactors. Each
// reactor subscribes to a domain fact (cart.paid, cart.refunded) and sends the
// buyer-facing transactional notification (receipt / refund email). It is
// idempotent — safe for at-least-once delivery with asynq retry + DLQ.
//
// Boundary this enforces (docs/domain-map.md §8): Notification REACTS to facts;
// it is NOT called inline in another domain's fan-out. The receipt used to be
// sent inside postcheckout.OnCartPaid (itself invoked from
// integration.ReactCartPaid); it now rides cart.paid as its own reactor, after
// the Order is materialised and the tracking token is set.
package listeners

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// ReceiptSender builds and sends the buyer-facing transactional emails. It is
// the slice of postcheckout.Service this reactor needs, declared locally (a
// consumer-side interface) so the Notification module depends on a behaviour,
// not a concrete type — and so main wires the existing implementation without an
// import cycle. The implementation owns the ordering guard (Order materialised +
// tracking token ready) and the exactly-once idempotency markers; this reactor
// only decides WHEN, in reaction to the fact.
type ReceiptSender interface {
	// SendPaidReceipt sends the "pagamento confirmado" receipt. It returns a
	// non-nil error ONLY when the send is not yet safe (Order not materialised or
	// tracking token not set) so the caller retries until it is; a delivered or
	// intentionally-skipped receipt returns nil.
	SendPaidReceipt(ctx context.Context, cartID string) error
	// SendRefundEmail sends the "pedido estornado" email. Idempotent; best-effort.
	SendRefundEmail(ctx context.Context, cartID string) error
}

// Listener reacts to cart payment facts and emits buyer notifications.
type Listener struct {
	receipts ReceiptSender
	logger   *zap.Logger
}

// New builds the Notification reactor over the given receipt sender.
func New(receipts ReceiptSender, log *zap.Logger) *Listener {
	return &Listener{receipts: receipts, logger: log.Named("notification.listener")}
}

// OnCartPaid reacts to cart.paid by sending the buyer receipt. The receipt
// depends on the materialised Order + tracking token; SendPaidReceipt returns a
// retryable error until both are ready (asynq re-tenta), and is idempotent so a
// redelivery never sends a second receipt.
func (l *Listener) OnCartPaid(ctx context.Context, cartID string) error {
	if err := l.receipts.SendPaidReceipt(ctx, cartID); err != nil {
		logger.From(ctx, l.logger).Debug("cart.paid receipt deferred, will retry",
			zap.String("cart_id", cartID), zap.Error(err))
		return err
	}
	return nil
}

// OnCartRefunded reacts to cart.refunded by sending the refund email. Idempotent
// via the order timeline marker inside SendRefundEmail.
func (l *Listener) OnCartRefunded(ctx context.Context, cartID string) error {
	return l.receipts.SendRefundEmail(ctx, cartID)
}

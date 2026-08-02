// Package listeners contains the Inventory module's event reactors. Each reactor
// subscribes to a domain fact (cart.paid) and reconciles the inventory-side state
// — here, the waitlist fulfilment. It is idempotent — safe for at-least-once
// delivery with asynq retry + DLQ.
//
// Boundary this enforces (docs/domain-map.md §3 Inventory, §8 handlers vs
// listeners): Inventory REACTS to facts; it is NOT called inline in another
// domain's fan-out. The waitlist fulfilment used to run inside
// postcheckout.OnCartPaid (itself invoked from integration.ReactCartPaid); it now
// rides cart.paid as its own reactor (Fatia A2).
package listeners

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// WaitlistFulfiller flips a cart's notified waitlist rows to fulfilled and emits
// the waitlist.fulfilled fact for each affected row. It is the slice of the
// Inventory repository this reactor needs, declared locally (a consumer-side
// interface) so the reactor depends on a behaviour, not a concrete type — and so
// the unit test wires a fake without a DB. The implementation owns the UPDATE
// (WHERE status='notified' → idempotent by construction) and the per-row event
// emission; this reactor only decides WHEN, in reaction to the fact.
type WaitlistFulfiller interface {
	// MarkWaitlistFulfilledByCart flips every 'notified' waitlist row tied to the
	// cart to 'fulfilled' (the buyer paid within the window) and emits one
	// waitlist.fulfilled per affected row. Rows in 'waiting' are left untouched. A
	// redelivery is a no-op (0 rows updated → 0 emissions).
	MarkWaitlistFulfilledByCart(ctx context.Context, cartID string) error
}

// Listener reacts to cart payment facts and reconciles the waitlist.
type Listener struct {
	fulfiller WaitlistFulfiller
	logger    *zap.Logger
}

// New builds the Inventory reactor over the given waitlist fulfiller.
func New(fulfiller WaitlistFulfiller, log *zap.Logger) *Listener {
	return &Listener{fulfiller: fulfiller, logger: log.Named("inventory.listener")}
}

// OnCartPaid reacts to cart.paid by fulfilling the cart's notified waitlist rows.
// The error is PROPAGATED (→ asynq retry) rather than swallowed; the operation is
// idempotent (WHERE status='notified'), so a retry after a partial failure never
// re-fulfils or re-emits an already-fulfilled row.
func (l *Listener) OnCartPaid(ctx context.Context, cartID string) error {
	if err := l.fulfiller.MarkWaitlistFulfilledByCart(ctx, cartID); err != nil {
		logger.From(ctx, l.logger).Warn("cart.paid waitlist fulfilment failed, will retry",
			zap.String("cart_id", cartID), zap.Error(err))
		return err
	}
	return nil
}

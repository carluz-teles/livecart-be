package listeners

import "context"

// OnCartCancelled reacts to cart.cancelled: it flips the Order — and its payment
// row — to 'cancelled'. Idempotent: a redelivery on an already-cancelled Order is
// a no-op.
//
// cart.cancelled has two producers (the payment-cancel webhook and the
// blocked-handle cancel); the latter carries no store_id, so storeID may be empty
// and is used only for log correlation (the Order is keyed by cart_id). Unpaid
// carts have no materialised Order — those return ErrOrderNotMaterialised, which
// the composition-root wiring treats as a benign skip.
func (l *Listener) OnCartCancelled(ctx context.Context, cartID, storeID string) error {
	return l.applyTerminalStatus(ctx, cartID, storeID, orderStatusCancelled)
}

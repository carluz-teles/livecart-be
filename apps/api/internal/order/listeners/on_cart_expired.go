package listeners

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// OnCartExpired transitions the Order from pending_payment → expired (terminal)
// when the cart expires without payment.
//
// Idempotent: if no pending_payment Order exists for the cart (already expired,
// already paid, or never created) → returns nil without error.
//
// Note: a cart that is paid can never emit cart.expired — ExpireCart guards
// against paying an expired cart and UpdateCartPayment rejects carts that are
// already expired, so we will only ever find Orders in pending_payment here.
func (l *Listener) OnCartExpired(ctx context.Context, cartID, storeID string) error {
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("order OnCartExpired: invalid cart_id %q: %w", cartID, err)
	}

	log := logger.From(ctx, l.logger).With(
		zap.String("cart_id", cartID),
		zap.String("store_id", storeID),
	)

	expiredID, err := l.queries.ExpireOrderByCartID(ctx, cid)
	if errors.Is(err, pgx.ErrNoRows) {
		// No pending_payment Order for this cart → no-op (idempotent).
		log.Debug("OnCartExpired: no pending_payment order found, nothing to do")
		return nil
	}
	if err != nil {
		return fmt.Errorf("order OnCartExpired: expiring order for cart %s: %w", cartID, err)
	}

	log.Info("OnCartExpired: order expired",
		zap.String("order_id", uuidStr(expiredID)),
	)
	return nil
}

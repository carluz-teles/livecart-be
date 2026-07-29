// Package listeners contains the Coupon module's event reactors. Each reactor
// subscribes to a domain fact (cart.paid, cart.refunded) and reconciles the
// coupon redemption tied to the cart — confirming it on payment (reserved →
// confirmed) and refunding it on chargeback (→ refunded, slot back to
// circulation). It is idempotent — safe for at-least-once delivery with asynq
// retry + DLQ.
//
// Boundary this enforces (docs/domain-map.md §8 handlers vs listeners): Coupon
// REACTS to facts; it is NOT called inline in another domain's fan-out. The
// redemption confirm/refund used to run inline inside integration.ReactCartPaid /
// ReactCartRefunded (via an injected syncer hook); it now rides cart.paid /
// cart.refunded as its own reactor.
package listeners

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// RedemptionConfirmer flips the cart's coupon redemption in reaction to a
// payment fact. It is the slice of coupon.Service this reactor needs, declared
// locally (a consumer-side interface) so the reactor depends on a behaviour, not
// a concrete type — and so the unit test wires a fake without a DB. The
// implementation owns the state guards (no-op unless the redemption is in the
// expected state) and the used_count bookkeeping; this reactor only decides
// WHEN, in reaction to the fact.
type RedemptionConfirmer interface {
	// ConfirmRedemption flips the cart's redemption from 'reserved' to
	// 'confirmed'. No-op when the cart has no redemption or it is not 'reserved'
	// (already confirmed / refunded / expired), so a redelivery never double-writes.
	ConfirmRedemption(ctx context.Context, cartID string) error
	// RefundRedemption flips the redemption to 'refunded' and returns the slot to
	// circulation. No-op when the cart has no redemption or it is already
	// 'refunded', so a redelivery never double-decrements the used_count.
	RefundRedemption(ctx context.Context, cartID string) error
}

// Listener reacts to cart payment facts and reconciles the coupon redemption.
type Listener struct {
	redemptions RedemptionConfirmer
	logger      *zap.Logger
}

// New builds the Coupon reactor over the given redemption confirmer.
func New(redemptions RedemptionConfirmer, log *zap.Logger) *Listener {
	return &Listener{redemptions: redemptions, logger: log.Named("coupon.listener")}
}

// OnCartPaid reacts to cart.paid by confirming the cart's coupon redemption. The
// error is PROPAGATED (→ asynq retry) rather than swallowed; the operation is
// idempotent (no-op unless the redemption is still 'reserved'), so a retry after
// a partial failure never re-confirms an already-confirmed redemption.
func (l *Listener) OnCartPaid(ctx context.Context, cartID string) error {
	if err := l.redemptions.ConfirmRedemption(ctx, cartID); err != nil {
		logger.From(ctx, l.logger).Warn("cart.paid coupon confirm failed, will retry",
			zap.String("cart_id", cartID), zap.Error(err))
		return err
	}
	return nil
}

// OnCartRefunded reacts to cart.refunded by refunding the cart's coupon
// redemption (returns the slot to circulation). The error is PROPAGATED (→ asynq
// retry); the operation is idempotent (no-op if there is no redemption or it is
// already 'refunded'), so a retry never double-decrements the used_count.
func (l *Listener) OnCartRefunded(ctx context.Context, cartID string) error {
	if err := l.redemptions.RefundRedemption(ctx, cartID); err != nil {
		logger.From(ctx, l.logger).Warn("cart.refunded coupon refund failed, will retry",
			zap.String("cart_id", cartID), zap.Error(err))
		return err
	}
	return nil
}

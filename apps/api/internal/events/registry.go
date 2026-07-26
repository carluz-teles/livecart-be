package events

import (
	"context"
	"encoding/json"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// RegisterHandlers wires every canonical event to its consumer. As flows
// migrate (Fases 04-11) their handlers are registered here. Fase 01 ships only
// the smoke-test handler.
func RegisterHandlers(mux *asynq.ServeMux, log *zap.Logger) {
	mux.HandleFunc(string(TestPing), handleTestPing(log))

	// Groups A/C — event/session/cart lifecycle. Observability-only for now;
	// richer consumers (notifications, analytics) arrive in later phases. Every
	// emitted event needs a handler or asynq reports "handler not found".
	mux.HandleFunc(string(EventEventCreated), logEvent(log))
	mux.HandleFunc(string(EventEventEnded), logEvent(log))
	mux.HandleFunc(string(SessionCreated), logEvent(log))
	mux.HandleFunc(string(SessionEnded), logEvent(log))
	mux.HandleFunc(string(PostWindowClosed), logEvent(log))
	mux.HandleFunc(string(CartItemAdded), logEvent(log))
	// CartCheckoutArmed, CartReopened, CartExpired and CartCancelled are registered
	// by the composition root (main.newApp) with domain handlers: checkout_armed/
	// reopened arm the cart.expire ETA timer (where expires_at is set — live carts
	// have no window until the event ends); cart.expired drives the ERP reversal
	// reactor; cart.cancelled drives the Order → 'cancelled' reactor.
	// Registering them here too would panic asynq on a duplicate pattern.

	// Group D — stock & waitlist. Observability-only for now.
	mux.HandleFunc(string(StockReserved), logEvent(log))
	mux.HandleFunc(string(StockReleased), logEvent(log))
	mux.HandleFunc(string(WaitlistQueued), logEvent(log))
	mux.HandleFunc(string(WaitlistNotified), logEvent(log))
	mux.HandleFunc(string(WaitlistFulfilled), logEvent(log))
	mux.HandleFunc(string(WaitlistExpired), logEvent(log))

	// Group E — payment. The canonical facts are cart.paid / cart.refunded
	// (fan-out anchors, registered by the composition root) and payment.failed
	// (terminal). Order timeline (group F) is internal (order_events rows), not on
	// the bus; the old funnel/telemetry facts were deprecated.
	mux.HandleFunc(string(PaymentFailed), logEvent(log))

	// Group O — Order post-payment facts (order.paid / order.refunded), emitted by
	// the Order domain (Fatia 11a). Observability-only no-op for now so asynq does
	// NOT report "handler not found" and dead-letter the task (an emitted event
	// with no registered handler fails). Fatia 11b registers the real ERP reactors
	// from the composition root (main.newApp) — which would panic asynq on a
	// duplicate pattern, so these two no-ops MUST be removed there, mirroring how
	// cart.paid/cart.refunded are wired only in main.newApp.
	mux.HandleFunc(string(OrderPaid), logEvent(log))
	mux.HandleFunc(string(OrderRefunded), logEvent(log))

	// Group J — billing/subscription facts. Observability-only for now;
	// TrialEndingSoon is NOT here — it is a scheduled command registered by the
	// composition root (main.newApp) with a domain handler.
	mux.HandleFunc(string(SubscriptionActivated), logEvent(log))
	mux.HandleFunc(string(SubscriptionPastDue), logEvent(log))
	mux.HandleFunc(string(SubscriptionGraceExpired), logEvent(log))
	mux.HandleFunc(string(SubscriptionCanceled), logEvent(log))
	mux.HandleFunc(string(SubscriptionPaused), logEvent(log))
	mux.HandleFunc(string(SubscriptionResumed), logEvent(log))
	mux.HandleFunc(string(GMVRecorded), logEvent(log))
	mux.HandleFunc(string(GMVRefunded), logEvent(log))

	// Group I — notifications (unified across channels; channel in the payload).
	mux.HandleFunc(string(NotificationRequested), logEvent(log))
	mux.HandleFunc(string(NotificationSent), logEvent(log))
	mux.HandleFunc(string(NotificationFailed), logEvent(log))
	mux.HandleFunc(string(NotificationSkipped), logEvent(log))

	// Group G — ERP / Tiny.
	mux.HandleFunc(string(ERPOrderCreated), logEvent(log))
	mux.HandleFunc(string(ERPOrderFinalized), logEvent(log))
	mux.HandleFunc(string(ERPOrderCancelled), logEvent(log))
	mux.HandleFunc(string(ERPFinalizationFailed), logEvent(log))

	// Group H — shipping / delivery.
	mux.HandleFunc(string(ShipmentCreated), logEvent(log))
	mux.HandleFunc(string(TrackingCodeGenerated), logEvent(log))
	mux.HandleFunc(string(ShipmentStatusUpdated), logEvent(log))
	mux.HandleFunc(string(DeliveryConfirmed), logEvent(log))

	// Group K — platform / account.
	mux.HandleFunc(string(StoreCreated), logEvent(log))
	mux.HandleFunc(string(StoreSettingsUpdated), logEvent(log))
	mux.HandleFunc(string(MemberInvited), logEvent(log))
	mux.HandleFunc(string(MemberInviteAccepted), logEvent(log))
	mux.HandleFunc(string(MemberInviteRevoked), logEvent(log))
	mux.HandleFunc(string(MemberInviteResent), logEvent(log))
	mux.HandleFunc(string(MemberRoleChanged), logEvent(log))
	mux.HandleFunc(string(MemberRemoved), logEvent(log))
	mux.HandleFunc(string(UserSignedUp), logEvent(log))
	mux.HandleFunc(string(UserUpdated), logEvent(log))
	mux.HandleFunc(string(UserDeleted), logEvent(log))
	mux.HandleFunc(string(CustomerUpserted), logEvent(log))
	mux.HandleFunc(string(CouponApplied), logEvent(log))
	mux.HandleFunc(string(CouponConfirmed), logEvent(log))
	mux.HandleFunc(string(CouponRefunded), logEvent(log))
}

// logEvent is a generic observability consumer: it records that a canonical
// event was delivered, correlated by trace_id via logger.From.
func logEvent(log *zap.Logger) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var env Envelope
		if err := json.Unmarshal(t.Payload(), &env); err != nil {
			return asynq.SkipRetry
		}
		logger.From(ctx, log).Info("event observed",
			zap.String("event", string(env.Name)),
			zap.String("event_id", env.EventID),
			zap.String("source", string(env.Source)),
		)
		return nil
	}
}

// handleTestPing is a no-op consumer that proves the pipeline end to end:
// publish -> Redis -> worker -> handler. It logs via logger.From so store and
// trace context (once Fase 02 lands) show up automatically.
func handleTestPing(log *zap.Logger) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var env Envelope
		if err := json.Unmarshal(t.Payload(), &env); err != nil {
			// Non-retryable: a malformed payload will never parse.
			return asynq.SkipRetry
		}
		logger.From(ctx, log).Info("test.ping received",
			zap.String("event_id", env.EventID),
			zap.String("source", string(env.Source)),
		)
		return nil
	}
}

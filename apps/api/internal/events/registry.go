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
//
// exporter is the New Relic telemetry pipeline (telemetry project, Fatia 2/3).
// It may be nil (no NEW_RELIC_LICENSE_KEY at boot) — every call site below
// guards on that. A handful of facts feed it via the sync logAndExport wired
// here (event.created, session.created, session.ended, payment.failed,
// gmv.recorded, gmv.refunded); cart.paid, cart.refunded, cart.checkout_armed,
// cart.expired, cart.cancelled, event.ended and comment.received feed it too
// but are dispatched from the composition root (main.newApp) — either because
// that pattern is already claimed there (double registration panics asynq) or
// because their handler runs business fan-out telemetry must not share a
// budget with (see dispatchTelemetryAsync's doc) — see the comments below.
// comment.received additionally needs Dispatch called strictly AFTER its
// handler returns (not before, like the others) — see main.go's comment.received
// registration for why.
func RegisterHandlers(mux *asynq.ServeMux, log *zap.Logger, exporter Exporter) {
	mux.HandleFunc(string(TestPing), handleTestPing(log))

	// Groups A/C — event/session/cart lifecycle. Observability-only for now;
	// richer consumers (notifications, analytics) arrive in later phases. Every
	// emitted event needs a handler or asynq reports "handler not found".
	mux.HandleFunc(string(EventEventCreated), logAndExport(log, exporter))
	// EventEventEnded NÃO entra aqui: a composition root (main.newApp) registra
	// o reator que arma a morte da fila não atendida (RN-32) em "fim do evento
	// + carência". Registrar nos dois lugares faria o asynq entrar em pânico
	// por padrão duplicado. O exporter é chamado de lá também (mesmo motivo).
	mux.HandleFunc(string(SessionCreated), logAndExport(log, exporter))
	mux.HandleFunc(string(SessionEnded), logAndExport(log, exporter))
	mux.HandleFunc(string(PostWindowClosed), logEvent(log))
	mux.HandleFunc(string(CartItemAdded), logAndExport(log, exporter))
	// CartCancellationReverted NÃO entra aqui: a composition root (main.newApp)
	// registra o reactor que avisa o lojista no sino do painel. Registrar nos
	// dois lugares faria o asynq entrar em pânico por padrão duplicado.
	// CartCheckoutArmed, CartExpired and CartCancelled are registered by the
	// composition root (main.newApp) with domain handlers: checkout_armed arms
	// the cart.expire ETA timer (where expires_at is set — live carts have no
	// window until the event ends); cart.expired drives the ERP reversal
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
	mux.HandleFunc(string(PaymentFailed), logAndExport(log, exporter))

	// Group O — Order post-payment facts (order.paid / order.refunded), emitted by
	// the Order domain (Fatia 11a). These are NOT registered here: the composition
	// root (main.newApp) wires the real ERP reactors (ReactOrderPaidERP /
	// ReactOrderRefundedERP) onto them (Fatia 11b-2). asynq panics on a duplicate
	// pattern, so — exactly like cart.paid/cart.refunded — they must be claimed only
	// in main.newApp, never by this default registry.

	// Group J — billing/subscription facts. Observability-only for now;
	// TrialEndingSoon is NOT here — it is a scheduled command registered by the
	// composition root (main.newApp) with a domain handler.
	mux.HandleFunc(string(SubscriptionActivated), logEvent(log))
	mux.HandleFunc(string(SubscriptionPastDue), logEvent(log))
	mux.HandleFunc(string(SubscriptionGraceExpired), logEvent(log))
	mux.HandleFunc(string(SubscriptionCanceled), logEvent(log))
	mux.HandleFunc(string(SubscriptionPaused), logEvent(log))
	mux.HandleFunc(string(SubscriptionResumed), logEvent(log))
	// GMVRecorded/GMVRefunded feed the telemetry exporter (Fatia 3 — New Relic
	// LiveCommercePayment{outcome: "recorded"/"refunded"}). Neither fact drives
	// any other business fan-out, so logAndExport (sync) is safe here — unlike
	// cart.paid/cart.refunded/event.ended, there is no business budget for the
	// telemetry call to compete with.
	mux.HandleFunc(string(GMVRecorded), logAndExport(log, exporter))
	mux.HandleFunc(string(GMVRefunded), logAndExport(log, exporter))

	// Group I — notifications (unified across channels; channel in the payload).
	mux.HandleFunc(string(NotificationRequested), logEvent(log))
	mux.HandleFunc(string(NotificationSent), logEvent(log))
	mux.HandleFunc(string(NotificationFailed), logAndExport(log, exporter))
	mux.HandleFunc(string(NotificationSkipped), logEvent(log))

	// Group G — ERP / Tiny.
	mux.HandleFunc(string(ERPOrderCreated), logEvent(log))
	mux.HandleFunc(string(ERPOrderFinalized), logEvent(log))
	mux.HandleFunc(string(ERPOrderCancelled), logEvent(log))
	mux.HandleFunc(string(ERPFinalizationFailed), logAndExport(log, exporter))

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

// logAndExport is logEvent plus a telemetry Dispatch (Fatia 2 — New Relic
// exporter). asynq allows exactly one handler per pattern, so the exporter
// can't be registered as a second, separate mux.HandleFunc for facts already
// owned by logEvent here — it is chained into the same handler instead. exporter
// may be nil (no telemetry pipeline configured at boot); Dispatch is then
// simply skipped. Dispatch itself never errors (see Exporter doc) so it can't
// affect this handler's asynq retry outcome.
func logAndExport(log *zap.Logger, exporter Exporter) asynq.HandlerFunc {
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
		if exporter != nil {
			exporter.Dispatch(ctx, env)
		}
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

package exporter

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

// eventSender is the slice of NRClient the listeners need. Declared on the
// consumer side (Go idiom) so tests can inject a fake and assert the payload
// New Relic would have received, without spinning up an httptest server —
// NRClient's HTTP/retry behaviour is tested separately in client_test.go.
type eventSender interface {
	SendEvent(ctx context.Context, event any) error
}

// Listeners routes canonical LiveCart facts to their New Relic custom event.
// Every On<Fato> method — and Dispatch — is nil-error by design: a telemetry
// outage must never fail the asynq task that carried the fact (see
// events.Exporter's doc). Failures are logged via logger.From and swallowed.
type Listeners struct {
	client   eventSender
	cfg      Config
	logger   *zap.Logger
	enricher *Enricher
}

// NewListeners builds Listeners. Safe to construct with a disabled cfg — every
// method checks cfg.Enabled first and no-ops, so the composition root can wire
// it unconditionally regardless of whether NEW_RELIC_LICENSE_KEY is set.
func NewListeners(client eventSender, cfg Config, log *zap.Logger) *Listeners {
	return &Listeners{client: client, cfg: cfg, logger: log}
}

// SetEnricher wires the Postgres-backed enrichment (Fatia 3). A setter, not a
// constructor param, because NewListeners runs before *sqlc.Queries exists in
// the composition root (main.go builds nrListeners before the DB pool/queries)
// — same pattern as payment.Service.SetCartPaymentGateway. nil is safe (and is
// exactly the Fatia 2 behavior): every call site below guards on it and simply
// skips the enrichment fields, still exporting the base payload.
func (l *Listeners) SetEnricher(e *Enricher) {
	l.enricher = e
}

// Dispatch implements events.Exporter: routes an envelope to its New Relic
// handler by canonical fact name. Facts this exporter doesn't care about are
// silently ignored (e.g. if a future caller reuses Dispatch on a wider fact
// set without updating this switch).
func (l *Listeners) Dispatch(ctx context.Context, env events.Envelope) {
	if !l.cfg.Enabled {
		return
	}
	switch env.Name {
	case events.EventEventCreated:
		l.OnEventCreated(ctx, env)
	case events.EventEventEnded:
		l.OnEventEnded(ctx, env)
	case events.SessionCreated:
		l.OnSessionCreated(ctx, env)
	case events.SessionEnded:
		l.OnSessionEnded(ctx, env)
	case events.CartItemAdded:
		l.OnCartItemAdded(ctx, env)
	case events.CartPaid:
		l.OnCartPaid(ctx, env)
	case events.PaymentFailed:
		l.OnPaymentFailed(ctx, env)
	case events.CartCheckoutArmed:
		l.OnCartCheckoutArmed(ctx, env)
	case events.CartExpired:
		l.OnCartExpired(ctx, env)
	case events.CartCancelled:
		l.OnCartCancelled(ctx, env)
	case events.CartRefunded:
		l.OnCartRefunded(ctx, env)
	case events.GMVRecorded:
		l.OnGMVRecorded(ctx, env)
	case events.GMVRefunded:
		l.OnGMVRefunded(ctx, env)
	case events.CommentReceived:
		l.OnCommentReceived(ctx, env)
	case events.ERPFinalizationFailed:
		l.OnERPFinalizationFailed(ctx, env)
	case events.NotificationFailed:
		l.OnNotificationFailed(ctx, env)
	}
}

// OnEventCreated exports event.created as LiveCommerceEvent{action: "created"}.
func (l *Listeners) OnEventCreated(ctx context.Context, env events.Envelope) {
	var p struct {
		StoreID string `json:"store_id"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	l.sendLifecycleEvent(ctx, env, p.StoreID, "", "", "created")
}

// OnEventEnded exports event.ended as LiveCommerceEvent{action: "ended"}.
func (l *Listeners) OnEventEnded(ctx context.Context, env events.Envelope) {
	var p struct {
		StoreID string `json:"store_id"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	l.sendLifecycleEvent(ctx, env, p.StoreID, "", "", "ended")
}

// OnSessionCreated exports session.created as LiveCommerceEvent{action:
// "created"}. session.created's payload carries no store_id today (see
// live.emitSessionCreated) — StoreID is left empty; enrichment is out of scope
// for this fatia.
func (l *Listeners) OnSessionCreated(ctx context.Context, env events.Envelope) {
	var p struct {
		SessionID string `json:"session_id"`
		Platform  string `json:"platform"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	l.sendLifecycleEvent(ctx, env, "", p.SessionID, p.Platform, "created")
}

// OnSessionEnded exports session.ended as LiveCommerceEvent{action: "ended"}.
// session.ended's payload carries neither store_id nor platform today (see
// live.EndSession) — both are left empty; enrichment is out of scope for this
// fatia.
func (l *Listeners) OnSessionEnded(ctx context.Context, env events.Envelope) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	l.sendLifecycleEvent(ctx, env, "", p.SessionID, "", "ended")
}

// OnCartPaid exports cart.paid as LiveCommerceCart{status: "paid"},
// LiveCommercePayment{outcome: "approved"} and one LiveCommerceCartItem per
// line item — the fact carries a cart state transition and a payment outcome;
// the item breakdown is a Fatia 3 addition. session_id/platform aren't in the
// cart.paid payload today (see payment.Service.ProcessPaymentNotification) —
// left zero-value. discount_cents/shipping_cents/checkout_duration_ms and the
// item breakdown come from Enricher (nil-safe: skipped when not wired, e.g. in
// tests — see SetEnricher's doc).
func (l *Listeners) OnCartPaid(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID    string `json:"cart_id"`
		StoreID   string `json:"store_id"`
		PaymentID string `json:"payment_id"`
		Method    string `json:"payment_method"`
		GMVCents  int64  `json:"gmv_cents"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	ts := occurredAtMillis(env)
	provider := string(env.Source)

	cart := NewLiveCommerceCartPayload(l.cfg.Environment)
	cart.LiveEventID = env.LiveEventID
	cart.StoreID = p.StoreID
	cart.CartID = p.CartID
	cart.Status = "paid"
	cart.GMVCents = p.GMVCents
	cart.PaymentMethod = p.Method
	cart.Provider = provider
	cart.Timestamp = ts
	if l.enricher != nil {
		if snap, ok := l.enricher.CartSnapshot(ctx, p.CartID); ok {
			cart.DiscountCents = snap.DiscountCents
			cart.ShippingCents = snap.ShippingCents
			cart.CheckoutDurationMs = snap.CheckoutDurationMs
		}
	}
	l.send(ctx, env, cart)

	payment := NewLiveCommercePaymentPayload(l.cfg.Environment)
	payment.LiveEventID = env.LiveEventID
	payment.StoreID = p.StoreID
	payment.CartID = p.CartID
	payment.PaymentID = p.PaymentID
	payment.Provider = provider
	payment.Method = p.Method
	payment.Outcome = "approved"
	payment.AmountCents = p.GMVCents
	payment.Timestamp = ts
	l.send(ctx, env, payment)

	if l.enricher != nil {
		for _, item := range l.enricher.CartItemBreakdown(ctx, p.CartID, env.LiveEventID, p.StoreID, l.cfg.Environment, ts) {
			l.send(ctx, env, item)
		}
	}
}

// OnPaymentFailed exports payment.failed as LiveCommercePayment{outcome:
// "rejected"}. The payload doesn't carry an amount for this fact (see
// payment.Service.ProcessPaymentNotification — gmv_cents is only computed on
// the "paid" branch) — AmountCents stays 0; that enrichment is Fatia 3's job.
func (l *Listeners) OnPaymentFailed(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID    string `json:"cart_id"`
		StoreID   string `json:"store_id"`
		PaymentID string `json:"payment_id"`
		Method    string `json:"payment_method"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	payment := NewLiveCommercePaymentPayload(l.cfg.Environment)
	payment.LiveEventID = env.LiveEventID
	payment.StoreID = p.StoreID
	payment.CartID = p.CartID
	payment.PaymentID = p.PaymentID
	payment.Provider = string(env.Source)
	payment.Method = p.Method
	payment.Outcome = "rejected"
	payment.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, payment)
}

// OnCartItemAdded exports cart.item_added as LiveCommerceCart{status:
// "item_added"} — the funnel's start-of-cart signal. There is no cart.created
// fact in the domain (carts are inserted directly, no event emitted): the
// first item_added for a cart is the earliest telemetry-visible proxy for "a
// cart exists". Fires once per cart+product added (repeatable, no dedup key —
// see checkout.Repository.cartMutationEventName's doc), so downstream NRQL
// must use uniqueCount(cart_id), not count(*), to size the funnel's mouth.
func (l *Listeners) OnCartItemAdded(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID string `json:"cart_id"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	cart := NewLiveCommerceCartPayload(l.cfg.Environment)
	cart.LiveEventID = env.LiveEventID
	cart.CartID = p.CartID
	cart.Status = "item_added"
	cart.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, cart)
}

// OnERPFinalizationFailed exports erp.finalization_failed as
// LiveCommerceOps{error_type: "erp.finalization_failed"} — closes the "Gap
// conhecido — Erros" this dashboard used to just warn about (see
// infra/terraform/newrelic/main.tf).
func (l *Listeners) OnERPFinalizationFailed(ctx context.Context, env events.Envelope) {
	var p struct {
		StoreID  string `json:"store_id"`
		CartID   string `json:"cart_id"`
		Provider string `json:"provider"`
		Reason   string `json:"reason"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	ops := NewLiveCommerceOpsPayload(l.cfg.Environment)
	ops.LiveEventID = env.LiveEventID
	ops.StoreID = p.StoreID
	ops.CartID = p.CartID
	ops.ErrorType = "erp.finalization_failed"
	ops.Provider = p.Provider
	ops.ErrorMessage = p.Reason
	ops.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, ops)
}

// OnNotificationFailed exports notification.failed as LiveCommerceOps{error_type:
// "notification.failed"} — covers every channel (Channel carries which one:
// whatsapp, instagram_dm, ...), same gap-closing rationale as
// OnERPFinalizationFailed.
func (l *Listeners) OnNotificationFailed(ctx context.Context, env events.Envelope) {
	var p struct {
		StoreID string `json:"store_id"`
		Channel string `json:"channel"`
		CartID  string `json:"cart_id"`
		Error   string `json:"error"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	ops := NewLiveCommerceOpsPayload(l.cfg.Environment)
	ops.LiveEventID = env.LiveEventID
	ops.StoreID = p.StoreID
	ops.CartID = p.CartID
	ops.ErrorType = "notification.failed"
	ops.Channel = p.Channel
	ops.ErrorMessage = p.Error
	ops.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, ops)
}

// OnCartCheckoutArmed exports cart.checkout_armed as
// LiveCommerceCart{status: "checkout_armed"}. Dispatched async by the
// composition root (see main.go's armCartExpiry) — that handler arms the
// cart.expire ETA timer, business logic telemetry must not compete with.
func (l *Listeners) OnCartCheckoutArmed(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID string `json:"cart_id"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	cart := NewLiveCommerceCartPayload(l.cfg.Environment)
	cart.LiveEventID = env.LiveEventID
	cart.CartID = p.CartID
	cart.Status = "checkout_armed"
	cart.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, cart)
}

// OnCartExpired exports cart.expired as LiveCommerceCart{status: "expired"}.
// Dispatched async by the composition root (see main.go's cart.expired
// handler) — that handler runs the coupon release + ERP reversal reactors.
func (l *Listeners) OnCartExpired(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID  string `json:"cart_id"`
		StoreID string `json:"store_id"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	cart := NewLiveCommerceCartPayload(l.cfg.Environment)
	cart.LiveEventID = env.LiveEventID
	cart.StoreID = p.StoreID
	cart.CartID = p.CartID
	cart.Status = "expired"
	cart.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, cart)
}

// OnCartCancelled exports cart.cancelled as LiveCommerceCart{status:
// "cancelled"}. Dispatched async by the composition root (see main.go's
// cart.cancelled handler) — that handler runs coupon release, the ERP
// reversal (store cancellations) and the Order -> cancelled reactor.
func (l *Listeners) OnCartCancelled(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID  string `json:"cart_id"`
		StoreID string `json:"store_id"`
		Reason  string `json:"reason"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	cart := NewLiveCommerceCartPayload(l.cfg.Environment)
	cart.LiveEventID = env.LiveEventID
	cart.StoreID = p.StoreID
	cart.CartID = p.CartID
	cart.Status = "cancelled"
	cart.Reason = p.Reason
	cart.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, cart)
}

// OnCartRefunded exports cart.refunded as both LiveCommerceCart{status:
// "refunded"} and LiveCommercePayment{outcome: "refunded"} — mirrors
// OnCartPaid's dual export for the inverse transition. Dispatched async by the
// composition root (see main.go's cart.refunded handler) — that handler flips
// the Order, refunds the coupon and runs the customer-facing fan-out
// (ReactCartRefunded, refund email).
func (l *Listeners) OnCartRefunded(ctx context.Context, env events.Envelope) {
	var p struct {
		CartID    string `json:"cart_id"`
		StoreID   string `json:"store_id"`
		PaymentID string `json:"payment_id"`
		Method    string `json:"payment_method"`
		GMVCents  int64  `json:"gmv_cents"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	ts := occurredAtMillis(env)
	provider := string(env.Source)

	cart := NewLiveCommerceCartPayload(l.cfg.Environment)
	cart.LiveEventID = env.LiveEventID
	cart.StoreID = p.StoreID
	cart.CartID = p.CartID
	cart.Status = "refunded"
	cart.GMVCents = p.GMVCents
	cart.PaymentMethod = p.Method
	cart.Provider = provider
	cart.Timestamp = ts
	l.send(ctx, env, cart)

	payment := NewLiveCommercePaymentPayload(l.cfg.Environment)
	payment.LiveEventID = env.LiveEventID
	payment.StoreID = p.StoreID
	payment.CartID = p.CartID
	payment.PaymentID = p.PaymentID
	payment.Provider = provider
	payment.Method = p.Method
	payment.Outcome = "refunded"
	payment.AmountCents = p.GMVCents
	payment.Timestamp = ts
	l.send(ctx, env, payment)
}

// OnGMVRecorded exports gmv.recorded as LiveCommercePayment{outcome:
// "recorded"} — the billing ledger sale entry, carrying FeeCents/Billable
// straight from the payload (billing.Service.OnCartPaid) and Plan from
// Enricher.LedgerPlan (not in the payload — see its doc). Registered as a
// sync logAndExport handler (registry.go): unlike the cart lifecycle facts
// above, this fact drives no business fan-out of its own — telemetry export
// IS the entire handler, so there is no business budget to protect.
func (l *Listeners) OnGMVRecorded(ctx context.Context, env events.Envelope) {
	var p struct {
		StoreID     string `json:"store_id"`
		CartID      string `json:"cart_id"`
		AmountCents int64  `json:"amount_cents"`
		FeeCents    int64  `json:"fee_cents"`
		Billable    bool   `json:"billable"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	payment := NewLiveCommercePaymentPayload(l.cfg.Environment)
	payment.LiveEventID = env.LiveEventID
	payment.StoreID = p.StoreID
	payment.CartID = p.CartID
	payment.Outcome = "recorded"
	payment.AmountCents = p.AmountCents
	payment.FeeCents = p.FeeCents
	payment.Billable = &p.Billable
	payment.Timestamp = occurredAtMillis(env)
	if l.enricher != nil {
		if info, ok := l.enricher.LedgerPlan(ctx, p.CartID); ok {
			payment.Plan = info.Plan
		}
	}
	l.send(ctx, env, payment)
}

// OnGMVRefunded exports gmv.refunded as LiveCommercePayment{outcome:
// "refunded"} — the billing ledger fee-credit entry. Same sync registration
// rationale as OnGMVRecorded.
func (l *Listeners) OnGMVRefunded(ctx context.Context, env events.Envelope) {
	var p struct {
		StoreID        string `json:"store_id"`
		CartID         string `json:"cart_id"`
		AmountCents    int64  `json:"amount_cents"`
		FeeCreditCents int64  `json:"fee_credit_cents"`
	}
	if !l.decode(ctx, env, &p) {
		return
	}
	payment := NewLiveCommercePaymentPayload(l.cfg.Environment)
	payment.LiveEventID = env.LiveEventID
	payment.StoreID = p.StoreID
	payment.CartID = p.CartID
	payment.Outcome = "refunded"
	payment.AmountCents = p.AmountCents
	payment.FeeCents = p.FeeCreditCents
	payment.Timestamp = occurredAtMillis(env)
	if l.enricher != nil {
		if info, ok := l.enricher.LedgerPlan(ctx, p.CartID); ok {
			payment.Plan = info.Plan
			payment.Billable = &info.Billable
		}
	}
	l.send(ctx, env, payment)
}

// OnCommentReceived exports comment.received as LiveCommerceComment, the
// comment→cart conversion signal (Fatia 4). Fed by the correlation join
// (Enricher.CommentCartCorrelation): converted_to_cart=true + cart_id +
// time_to_cart_ms when a cart was created for the same event+buyer within
// 120s of the comment, converted_to_cart=false otherwise. "Converted to
// PAYMENT" is NOT computed here — it's a downstream NRQL funnel() joining
// this fact with LiveCommercePayment by cart_id.
//
// ORDERING (avoids the race the brief called out): comment.received's ONLY
// consumer, ProcessInstagramComment (main.go), creates the cart SYNCHRONOUSLY
// in the same call — there is no separate outbox hop between "comment saved"
// and "cart created". Dispatch for this fact is therefore called AFTER
// ProcessInstagramComment returns (see main.go), never before like
// cart.paid's dispatchTelemetryAsync call site — dispatching earlier would
// race the correlation query against a cart row that doesn't exist yet.
// Still async/detached (dispatchTelemetryAsync), so New Relic never shares
// budget with the comment ingestion path.
//
// The envelope payload (live.ProcessInstagramCommentInput) carries no
// event_id/store_id/has_purchase_intent/etc — those are resolved INSIDE
// ProcessInstagramComment and only persisted onto the live_comments row, not
// echoed back onto the bus. So this handler re-reads the row it just wrote,
// by platform_comment_id (env.Metadata["comment_id"], set by
// live.DispatchCommentReceived) — the row is the source of truth and is
// guaranteed to already exist given the ordering above.
func (l *Listeners) OnCommentReceived(ctx context.Context, env events.Envelope) {
	if l.enricher == nil {
		return
	}
	commentID := env.Metadata["comment_id"]
	if commentID == "" {
		return
	}
	correlation, ok := l.enricher.CommentCartCorrelation(ctx, commentID)
	if !ok {
		// No persisted live_comments row (e.g. the comment was dropped before
		// CreateLiveComment — no active session/event for the media_id) or a
		// query failure — Enricher already logged it. Nothing to export.
		return
	}

	p := NewLiveCommerceCommentPayload(l.cfg.Environment)
	p.LiveEventID = correlation.LiveEventID
	p.StoreID = correlation.StoreID
	p.SessionID = correlation.SessionID
	p.Platform = correlation.Platform
	p.PlatformUserID = correlation.PlatformUserID
	p.HasPurchaseIntent = correlation.HasPurchaseIntent
	p.MatchedProductID = correlation.MatchedProductID
	p.Result = correlation.Result
	p.ConvertedToCart = correlation.ConvertedToCart
	p.CartID = correlation.CartID
	p.TimeToCartMs = correlation.TimeToCartMs
	p.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, p)
}

// sendLifecycleEvent builds and sends a LiveCommerceEvent payload — shared by
// the four event/session lifecycle handlers.
func (l *Listeners) sendLifecycleEvent(ctx context.Context, env events.Envelope, storeID, sessionID, platform, action string) {
	p := NewLiveCommerceEventPayload(l.cfg.Environment)
	p.LiveEventID = env.LiveEventID
	p.StoreID = storeID
	p.SessionID = sessionID
	p.Platform = platform
	p.Action = action
	p.Timestamp = occurredAtMillis(env)
	l.send(ctx, env, p)
}

// decode unmarshals env.Payload into dst, logging (and returning false) on
// failure so callers can bail without exporting a half-built event.
func (l *Listeners) decode(ctx context.Context, env events.Envelope, dst any) bool {
	if err := json.Unmarshal(env.Payload, dst); err != nil {
		logger.From(ctx, l.logger).Warn("new relic exporter: decoding payload failed",
			zap.String("event", string(env.Name)),
			zap.String("event_id", env.EventID),
			zap.Error(err),
		)
		return false
	}
	return true
}

// send posts event to New Relic, logging (never returning) any failure — see
// events.Exporter's single-responsibility doc: telemetry can't break the
// pipeline that fed it.
func (l *Listeners) send(ctx context.Context, env events.Envelope, event any) {
	if err := l.client.SendEvent(ctx, event); err != nil {
		logger.From(ctx, l.logger).Error("new relic exporter: sending event failed",
			zap.String("event", string(env.Name)),
			zap.String("event_id", env.EventID),
			zap.Error(err),
		)
	}
}

// occurredAtMillis converts the envelope's OccurredAt to epoch milliseconds,
// the timestamp format the New Relic Event API expects.
func occurredAtMillis(env events.Envelope) int64 {
	return env.OccurredAt.UnixMilli()
}

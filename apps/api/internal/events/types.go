// Package events is the backbone of LiveCart's asynchronous, event-driven
// architecture. Domain transitions publish canonical events (via a
// transactional outbox) that fan out to idempotent consumers over an asynq +
// Redis task queue.
//
// Design rules (see docs/async-events-analysis.md):
//   - Event names are canonical DOMAIN facts ("comment.received",
//     "cart.paid"), never implementation details. The origin (which
//     webhook/provider/channel dispatched it) travels as Envelope.Source /
//     Envelope.Metadata, not as a distinct event type.
//   - Webhooks are thin adapters: they validate, dedup by the provider event id
//     and dispatch the canonical event. Domain logic runs in the consumer.
package events

import (
	"encoding/json"
	"time"
)

// Name is a canonical domain event name, e.g. "comment.received".
type Name string

// Canonical event names. Domain facts, never implementation details — the
// origin travels in Envelope.Source. This list grows as flows migrate.
const (
	// TestPing is a no-op event used to prove the pipeline end to end.
	TestPing Name = "test.ping"

	// Live / session / event lifecycle (group A).
	EventEventCreated Name = "event.created"
	EventEventEnded   Name = "event.ended"
	SessionCreated    Name = "session.created"
	SessionEnded      Name = "session.ended"
	PostWindowClosed  Name = "post.window_closed"

	// Comment ingestion (group B). One canonical event; the platform/channel
	// (live, story, dm) is Envelope.Source.
	CommentReceived Name = "comment.received"

	// MessageReceived: uma DM do Instagram chegou. Irmão de CommentReceived e
	// pelo mesmo motivo — a borda HTTP grava o fato e responde 200; o trabalho
	// (resposta de story, entrega de carrinho pendente, auditoria) roda no
	// consumidor.
	//
	// Antes disto o caminho de DM processava tudo DENTRO do request: dois
	// lookups, refresh de token e até duas chamadas à Graph, com timeout de 30s
	// cada. A Meta exige 200 em ≤5s e desinscreve o app após 1 hora de falha
	// contínua; o pior caso passava disso com folga.
	MessageReceived Name = "message.received"

	// Cart lifecycle (group C).
	CartItemAdded     Name = "cart.item_added"
	CartCheckoutArmed Name = "cart.checkout_armed"
	CartExpired       Name = "cart.expired"
	CartCancelled     Name = "cart.cancelled"
	// CartCancellationReverted: o pagamento chegou DEPOIS do cancelamento manual
	// do lojista e venceu — o cart voltou para pago (regra "pagamento vence").
	// Fato de auditoria: quem consome o cart.cancelled anterior precisa saber que
	// ele foi desfeito, e o lojista precisa do rastro de por que o pedido
	// "cancelado" reapareceu pago.
	CartCancellationReverted Name = "cart.cancellation_reverted"
	// CartPaid / CartRefunded are the canonical payment facts (specific-fact
	// strategy): the payment consumer resolves the provider status and emits the
	// right one; reactors (GMV, order, coupon, ...) subscribe to what they need.
	// They replace the payment.succeeded/payment.refunded telemetry facts.
	CartPaid     Name = "cart.paid"
	CartRefunded Name = "cart.refunded"

	// Stock & waitlist (group D).
	StockReserved     Name = "stock.reserved"
	StockReleased     Name = "stock.released"
	WaitlistQueued    Name = "waitlist.queued"
	WaitlistNotified  Name = "waitlist.notified"
	WaitlistFulfilled Name = "waitlist.fulfilled"
	WaitlistExpired   Name = "waitlist.expired"

	// Checkout & payment (group E). The provider (pagarme/mercadopago/stripe)
	// travels as Envelope.Source. The canonical payment facts are cart.paid /
	// cart.refunded (group C); payment.failed is the terminal rejection fact.
	// The old funnel/telemetry facts (checkout.initiated, pix.generated/expired,
	// payment.processing/succeeded/refunded/chargeback) were deprecated once the
	// payment choreography made cart.paid/cart.refunded the fan-out anchors.
	PaymentFailed Name = "payment.failed"

	// Order timeline (group F) is INTERNAL to the Order Service: the transitions
	// live as order_events rows (UNIQUE(cart_id, event_type)), not as bus events.
	// Nothing on the bus consumed them (notifications key off cart.* / shipment.*),
	// so order.payment_confirmed/cancelled/refunded/shipped/delivered were removed
	// from the vocabulary — InsertOrderEvent just writes the timeline row.

	// Order post-payment facts (group O). Emitted by the Order domain AFTER the
	// immutable Order is materialised (order.paid) or flipped to refunded
	// (order.refunded), transactionally via the outbox with dedup by order_id
	// (exactly-once). They are the fan-out anchors for POST-payment consumers:
	// the composition root (main.newApp) wires the ERP finalisation/refund
	// reactors onto them (ERP().OnOrderPaid / OnOrderRefunded), keeping the ERP
	// off cart.*. Distinct from group F, which are internal order_events timeline
	// ROWS — these are first-class BUS facts.
	OrderPaid     Name = "order.paid"
	OrderRefunded Name = "order.refunded"

	// CartExpire is an internal SCHEDULED COMMAND (not a domain fact): enqueued
	// with asynq.ProcessAt(expires_at) so the cart expires exactly at its window
	// instead of waiting for the 5-min sweep. Its handler runs the same guarded
	// ExpireCart; the sweep stays as a safety net for any lost task.
	CartExpire Name = "cart.expire"

	// PaymentProcess is a COMMAND (L1→L2 inversion): the payment webhook is a thin
	// dispatcher that emits it to the outbox; the consumer runs the guarded
	// ProcessPaymentNotification with retry + DLQ instead of a detached goroutine.
	// Emitted with an empty dedup_key on purpose — one payment fires several
	// webhooks (pending→paid, provider bursts) and each must re-reconcile; the
	// consumer is idempotent (guarded UpdateCartPaymentStatus).
	PaymentProcess Name = "payment.process"

	// EventWindowClose is an internal SCHEDULED COMMAND: enqueued with
	// ProcessAt(ends_at) for timed post/story events so they finalize exactly at
	// their window end (emitting post.window_closed + event.ended) instead of
	// waiting for the SweepEndedTimedEvents sweep, which stays as the backstop.
	EventWindowClose Name = "event.window_close"

	// EventWaitlistClose is an internal SCHEDULED COMMAND (RN-32): armed by the
	// event.ended reactor for "event end + grace" (the same grace the carts got
	// as their deadline). It kills the queue entries that were never fulfilled
	// so the waitlister's cart can finally expire — the ExpireCart guard
	// abstains for any cart holding a 'waiting' item, and that branch has no
	// deadline of its own, so without this the cart is eternal and keeps the
	// stock of its non-waitlisted items reserved forever.
	EventWaitlistClose Name = "event.waitlist_close"

	// SessionPublish is an internal SCHEDULED COMMAND (RN-31): enqueued with
	// ProcessAt(scheduled_for) so an Instagram post/reel/story goes live at the
	// hour the merchant chose. The scheduler HAS to be ours — the Content
	// Publishing API has no scheduled_publish_time and its media container
	// expires in 24h, so the container is only opened when the task fires.
	// Guard-first like every other ETA command: the handler CLAIMS the job row
	// (status 'scheduled' → 'publishing') and a cancelled job simply finds
	// nothing to claim. SweepDuePublishJobs is the backstop for a lost task.
	SessionPublish Name = "session.publish"

	// SubscriptionProcess is a COMMAND (billing L1→L2 inversion): the Stripe
	// webhook is a thin dispatcher that emits it to the outbox carrying the raw
	// event; the consumer runs the guarded ProcessWebhookEvent with asynq retry +
	// DLQ instead of processing synchronously in the request. Dedup by the Stripe
	// event id — Stripe redelivers the same id at-least-once, so the constraint
	// collapses redeliveries (and ProcessWebhookEvent is itself idempotent).
	SubscriptionProcess Name = "subscription.process"

	// Billing / subscription (group J, PRD 007). The subscription lifecycle
	// facts are emitted from the Stripe-webhook hub (applySubscription); GMV
	// facts from the metered-fee ledger.
	TrialEndingSoon          Name = "trial.ending_soon" // also the SCHEDULED COMMAND armed at trial_ends_at - N days
	SubscriptionActivated    Name = "subscription.activated"
	SubscriptionPastDue      Name = "subscription.past_due"
	SubscriptionGraceExpired Name = "subscription.grace_expired"
	// SubscriptionCanceled keeps the American spelling ON PURPOSE: it mirrors
	// Stripe's own vocabulary (subscription status "canceled"), the source of
	// this fact. This is the one intentional exception to the British "cancelled"
	// used by cart.cancelled / erp.order_cancelled — do not "fix" it.
	SubscriptionCanceled Name = "subscription.canceled"
	SubscriptionPaused   Name = "subscription.paused"
	SubscriptionResumed  Name = "subscription.resumed"
	GMVRecorded          Name = "gmv.recorded"
	GMVRefunded          Name = "gmv.refunded"

	// Notifications (group I). One canonical fact per outbound message lifecycle
	// across ALL channels; the channel (instagram_dm / whatsapp / email) travels in
	// the payload's `channel` field (Source stays internal). email.sent and
	// whatsapp.fallback_sent/failed were merged into notification.sent/failed so
	// the analytics exporter sees one vocabulary with channel as a dimension.
	NotificationRequested Name = "notification.requested"
	NotificationSent      Name = "notification.sent"
	NotificationFailed    Name = "notification.failed"
	NotificationSkipped   Name = "notification.skipped"

	// ERP / Tiny (group G). Best-effort facts around the resumable ERP order
	// state machine.
	ERPOrderCreated       Name = "erp.order_created"
	ERPOrderFinalized     Name = "erp.order_finalized"
	ERPOrderCancelled     Name = "erp.order_cancelled"
	ERPFinalizationFailed Name = "erp.finalization_failed"

	// Shipping / delivery (group H). Carrier-level facts, distinct from the
	// order timeline (order.shipped/delivered are group F).
	ShipmentCreated       Name = "shipment.created"
	TrackingCodeGenerated Name = "shipment.tracking_generated"
	ShipmentStatusUpdated Name = "shipment.status_updated"
	DeliveryConfirmed     Name = "shipment.delivered"

	// Platform / account (group K). store/member/user/customer/coupon lifecycle.
	StoreCreated         Name = "store.created"
	StoreSettingsUpdated Name = "store.settings_updated"
	MemberInvited        Name = "member.invited"
	MemberInviteAccepted Name = "member.invite_accepted"
	MemberInviteRevoked  Name = "member.invite_revoked"
	MemberInviteResent   Name = "member.invite_resent"
	MemberRoleChanged    Name = "member.role_changed"
	MemberRemoved        Name = "member.removed"
	UserSignedUp         Name = "user.signed_up"
	UserUpdated          Name = "user.updated"
	UserDeleted          Name = "user.deleted"
	CustomerUpserted     Name = "customer.upserted"
	CouponApplied        Name = "coupon.applied"
	CouponConfirmed      Name = "coupon.confirmed"
	CouponRefunded       Name = "coupon.refunded"
)

// Source identifies where an event was dispatched from. It is metadata on the
// envelope — the same canonical event can arrive from several sources.
type Source string

const (
	SourceInstagramLive  Source = "instagram_live"
	SourceInstagramStory Source = "instagram_story"
	SourceInstagramPost  Source = "instagram_post"
	SourceInstagramDM    Source = "instagram_dm"
	SourcePagarme        Source = "pagarme"
	SourceMercadoPago    Source = "mercadopago"
	SourceStripe         Source = "stripe"
	SourceClerk          Source = "clerk"
	// SourceInternal marks events emitted by internal domain transitions and
	// background workers (e.g. cart expiry) rather than an inbound webhook.
	SourceInternal Source = "internal"
)

// Queue names, ordered by priority. The asynq server weights them (see server.go).
const (
	QueueFastTrack = "fast-track" // low-latency, high-priority (session/event lifecycle)
	QueueNormal    = "normal"     // default for most domain events
	QueueBatch     = "batch"      // heavy, tolerant-of-delay aggregation work
)

// QueuePolicy is the default per-queue task processing policy applied when a
// publisher does not override retry/timeout explicitly.
type QueuePolicy struct {
	MaxRetry int
	Timeout  time.Duration
}

// DefaultPolicies maps each queue to its default processing policy.
var DefaultPolicies = map[string]QueuePolicy{
	QueueFastTrack: {MaxRetry: 3, Timeout: 5 * time.Second},
	QueueNormal:    {MaxRetry: 3, Timeout: 15 * time.Second},
	QueueBatch:     {MaxRetry: 1, Timeout: 60 * time.Second},
}

// Envelope is the canonical wire format for every event. It is serialized as
// the asynq task payload, so a consumer can reconstruct the domain payload, the
// origin metadata and the trace context.
type Envelope struct {
	// EventID uniquely identifies this event instance (idempotency + outbox key).
	EventID string `json:"event_id"`
	// SchemaVersion versions the payload shape so consumers can evolve without
	// breaking. Defaults to 1; bump when a payload's fields change incompatibly.
	SchemaVersion int `json:"schema_version"`
	// Name is the canonical domain event name.
	Name Name `json:"name"`
	// Source is where the event was dispatched from (webhook/provider/internal).
	Source Source `json:"source"`
	// Metadata carries origin details that are NOT part of the domain payload
	// (e.g. provider event id, channel), kept out of the event name on purpose.
	Metadata map[string]string `json:"metadata,omitempty"`
	// OccurredAt is when the domain transition happened.
	OccurredAt time.Time `json:"occurred_at"`
	// TraceID / SpanID carry W3C trace context so the consumer continues the
	// span started by the producer (Fase 02).
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
	// DedupKey lets consumers deduplicate at-least-once delivery when there is
	// no natural unique key on the side-effect.
	DedupKey string `json:"dedup_key,omitempty"`
	// Payload is the domain-specific data for this event.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Queue returns the queue this event should be enqueued on. For now every event
// rides the normal queue; later phases route lifecycle events to fast-track and
// aggregation to batch.
func (e Envelope) Queue() string {
	return QueueNormal
}

// Package events is the backbone of LiveCart's asynchronous, event-driven
// architecture. Domain transitions publish canonical events (via a
// transactional outbox) that fan out to idempotent consumers over an asynq +
// Redis task queue.
//
// Design rules (see docs/async-events-analysis.md):
//   - Event names are canonical DOMAIN facts ("comment.received",
//     "payment.succeeded"), never implementation details. The origin (which
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
	EventEventStarted Name = "event.started"
	EventEventEnded   Name = "event.ended"
	SessionCreated    Name = "session.created"
	SessionLive       Name = "session.live"
	SessionEnded      Name = "session.ended"
	PostWindowClosed  Name = "post.window_closed"

	// Comment ingestion (group B). One canonical event; the platform/channel
	// (live, story, dm) is Envelope.Source.
	CommentReceived Name = "comment.received"

	// Cart lifecycle (group C).
	CartCreated        Name = "cart.created"
	CartItemAdded      Name = "cart.item_added"
	CartItemQtyChanged Name = "cart.item_qty_changed"
	CartItemRemoved    Name = "cart.item_removed"
	CartCheckoutArmed  Name = "cart.checkout_armed"
	CartExpired        Name = "cart.expired"
	CartReopened       Name = "cart.reopened"
	CartCancelled      Name = "cart.cancelled"

	// Stock & waitlist (group D).
	StockReserved     Name = "stock.reserved"
	StockReleased     Name = "stock.released"
	WaitlistQueued    Name = "waitlist.queued"
	WaitlistNotified  Name = "waitlist.notified"
	WaitlistFulfilled Name = "waitlist.fulfilled"
	WaitlistExpired   Name = "waitlist.expired"

	// Checkout & payment (group E). The provider (pagarme/mercadopago/stripe)
	// travels as Envelope.Source; these are the canonical payment facts.
	CheckoutInitiated Name = "checkout.initiated"
	PixGenerated      Name = "pix.generated"
	PixExpired        Name = "pix.expired"
	PaymentProcessing Name = "payment.processing"
	PaymentSucceeded  Name = "payment.succeeded"
	PaymentFailed     Name = "payment.failed"
	PaymentRefunded   Name = "payment.refunded"
	PaymentChargeback Name = "payment.chargeback"

	// Order timeline (group F). One-to-one with the order_events rows, so the
	// UNIQUE(cart_id, event_type) guard makes emission idempotent by construction.
	OrderPaymentConfirmed Name = "order.payment_confirmed"
	OrderCancelled        Name = "order.cancelled"
	OrderRefunded         Name = "order.refunded"
	OrderShipped          Name = "order.shipped"
	OrderDelivered        Name = "order.delivered"

	// CartExpire is an internal SCHEDULED COMMAND (not a domain fact): enqueued
	// with asynq.ProcessAt(expires_at) so the cart expires exactly at its window
	// instead of waiting for the 5-min sweep. Its handler runs the same guarded
	// ExpireCart; the sweep stays as a safety net for any lost task.
	CartExpire Name = "cart.expire"
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

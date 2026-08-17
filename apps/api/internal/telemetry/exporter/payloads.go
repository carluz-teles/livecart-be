package exporter

// New Relic custom event types (the reserved "eventType" attribute). Schema
// fixed by the Architect for Fatia 2, enriched by Fatia 3 (checkout_duration_ms,
// fee_cents, plan, billable, discount_cents, shipping_cents, and the new
// LiveCommerceCartItem breakdown) — see each fatia's handoff doc.
const (
	eventTypeLiveCommerceCart     = "LiveCommerceCart"
	eventTypeLiveCommercePayment  = "LiveCommercePayment"
	eventTypeLiveCommerceEvent    = "LiveCommerceEvent"
	eventTypeLiveCommerceCartItem = "LiveCommerceCartItem"
	eventTypeLiveCommerceComment  = "LiveCommerceComment"
)

// LiveCommerceCartPayload is the New Relic custom event for a cart's payment
// state. Fatia 2 only ever emitted it with Status "paid" (OnCartPaid); Fatia 3
// adds Status "checkout_armed"/"expired"/"cancelled"/"refunded" from their own
// reactors, and enriches the "paid" event with DiscountCents/ShippingCents
// (from carts) and CheckoutDurationMs (carts.initial_snapshot_taken_at ->
// paid_at — see enrich.go's doc for why that column, not a dedicated
// "checkout_armed_at", is the duration anchor).
type LiveCommerceCartPayload struct {
	EventType          string `json:"eventType"`
	LiveEventID        string `json:"live_event_id,omitempty"`
	StoreID            string `json:"store_id,omitempty"`
	SessionID          string `json:"session_id,omitempty"`
	CartID             string `json:"cart_id,omitempty"`
	Platform           string `json:"platform,omitempty"`
	Status             string `json:"status,omitempty"`
	GMVCents           int64  `json:"gmv_cents,omitempty"`
	DiscountCents      int64  `json:"discount_cents,omitempty"`
	ShippingCents      int64  `json:"shipping_cents,omitempty"`
	CheckoutDurationMs int64  `json:"checkout_duration_ms,omitempty"`
	PaymentMethod      string `json:"payment_method,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Reason             string `json:"reason,omitempty"`
	// Environment is "staging"/"production"/"development" (config.Environment())
	// — staging and production share the same New Relic account (8291202) and
	// dashboard, so every custom event must self-identify to be filterable.
	Environment string `json:"environment"`
	Timestamp   int64  `json:"timestamp"`
}

// NewLiveCommerceCartPayload builds the payload with EventType prefilled.
func NewLiveCommerceCartPayload(environment string) LiveCommerceCartPayload {
	return LiveCommerceCartPayload{EventType: eventTypeLiveCommerceCart, Environment: environment}
}

// LiveCommercePaymentPayload is the New Relic custom event for a payment
// outcome: "approved" (cart.paid), "rejected" (payment.failed), "recorded"
// (gmv.recorded — the billing ledger sale, carrying FeeCents/Plan/Billable) or
// "refunded" (gmv.refunded — the fee credit).
//
// IMPORTANT — double-counting risk: a single paid cart emits TWO
// LiveCommercePayment events correlated by cart_id — one with
// outcome="approved" (from cart.paid, no fee_cents/plan) and one with
// outcome="recorded" (from gmv.recorded, carrying fee_cents/plan/billable).
// Any NRQL that sums amount_cents for GMV must filter to a single outcome
// (e.g. WHERE outcome != 'recorded', or WHERE outcome = 'approved') — summing
// amount_cents across outcomes for the same cart_id double-counts the GMV.
type LiveCommercePaymentPayload struct {
	EventType   string `json:"eventType"`
	LiveEventID string `json:"live_event_id,omitempty"`
	StoreID     string `json:"store_id,omitempty"`
	CartID      string `json:"cart_id,omitempty"`
	PaymentID   string `json:"payment_id,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Method      string `json:"method,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	FeeCents    int64  `json:"fee_cents,omitempty"`
	Plan        string `json:"plan,omitempty"`
	// Billable is a pointer so the New Relic event distinguishes "resolved
	// false" (a real business state — e.g. a trial-store sale that isn't
	// billable) from "not resolved" (enrichment didn't run/failed). A plain
	// bool with `omitempty` would drop billable=false from the exported
	// event, making it indistinguishable from missing data.
	Billable *bool `json:"billable,omitempty"`
	// Environment is "staging"/"production"/"development" (config.Environment())
	// — staging and production share the same New Relic account (8291202) and
	// dashboard, so every custom event must self-identify to be filterable.
	Environment string `json:"environment"`
	Timestamp   int64  `json:"timestamp"`
}

// NewLiveCommercePaymentPayload builds the payload with EventType prefilled.
func NewLiveCommercePaymentPayload(environment string) LiveCommercePaymentPayload {
	return LiveCommercePaymentPayload{EventType: eventTypeLiveCommercePayment, Environment: environment}
}

// LiveCommerceCartItemPayload is the New Relic custom event for a single line
// item of a paid cart (GMV breakdown by product) — one event per cart_items
// row, emitted alongside the "paid" LiveCommerceCart/LiveCommercePayment pair.
type LiveCommerceCartItemPayload struct {
	EventType      string `json:"eventType"`
	LiveEventID    string `json:"live_event_id,omitempty"`
	StoreID        string `json:"store_id,omitempty"`
	CartID         string `json:"cart_id,omitempty"`
	ProductID      string `json:"product_id,omitempty"`
	ProductName    string `json:"product_name,omitempty"`
	Quantity       int64  `json:"quantity,omitempty"`
	UnitPriceCents int64  `json:"unit_price_cents,omitempty"`
	RevenueCents   int64  `json:"revenue_cents,omitempty"`
	// Environment is "staging"/"production"/"development" (config.Environment())
	// — staging and production share the same New Relic account (8291202) and
	// dashboard, so every custom event must self-identify to be filterable.
	Environment string `json:"environment"`
	Timestamp   int64  `json:"timestamp"`
}

// NewLiveCommerceCartItemPayload builds the payload with EventType prefilled.
func NewLiveCommerceCartItemPayload(environment string) LiveCommerceCartItemPayload {
	return LiveCommerceCartItemPayload{EventType: eventTypeLiveCommerceCartItem, Environment: environment}
}

// LiveCommerceEventPayload is the New Relic custom event covering both the
// live-event and the session lifecycle (event.created/ended, session.created/
// ended) — Action distinguishes created vs ended, LiveEventID/SessionID which
// entity fired it.
type LiveCommerceEventPayload struct {
	EventType   string `json:"eventType"`
	LiveEventID string `json:"live_event_id,omitempty"`
	StoreID     string `json:"store_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Action      string `json:"action,omitempty"`
	// Environment is "staging"/"production"/"development" (config.Environment())
	// — staging and production share the same New Relic account (8291202) and
	// dashboard, so every custom event must self-identify to be filterable.
	Environment string `json:"environment"`
	Timestamp   int64  `json:"timestamp"`
}

// NewLiveCommerceEventPayload builds the payload with EventType prefilled.
func NewLiveCommerceEventPayload(environment string) LiveCommerceEventPayload {
	return LiveCommerceEventPayload{EventType: eventTypeLiveCommerceEvent, Environment: environment}
}

// LiveCommerceCommentPayload is the New Relic custom event for a live comment
// (comment.received), carrying the comment↔cart conversion signal Fatia 4
// adds: whether the buyer's cart was created within 120s of the comment
// (ConvertedToCart/CartID/TimeToCartMs). "Converted to PAYMENT" (not just
// cart) is deliberately NOT computed here — it is derived downstream via NRQL
// funnel() joining LiveCommerceComment and LiveCommercePayment by cart_id.
type LiveCommerceCommentPayload struct {
	EventType         string `json:"eventType"`
	LiveEventID       string `json:"live_event_id,omitempty"`
	StoreID           string `json:"store_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	Platform          string `json:"platform,omitempty"`
	PlatformUserID    string `json:"platform_user_id,omitempty"`
	HasPurchaseIntent bool   `json:"has_purchase_intent,omitempty"`
	MatchedProductID  string `json:"matched_product_id,omitempty"`
	Result            string `json:"result,omitempty"`
	ConvertedToCart   bool   `json:"converted_to_cart"`
	CartID            string `json:"cart_id,omitempty"`
	TimeToCartMs      int64  `json:"time_to_cart_ms,omitempty"`
	// Environment is "staging"/"production"/"development" (config.Environment())
	// — staging and production share the same New Relic account (8291202) and
	// dashboard, so every custom event must self-identify to be filterable.
	Environment string `json:"environment"`
	Timestamp   int64  `json:"timestamp"`
}

// NewLiveCommerceCommentPayload builds the payload with EventType prefilled.
func NewLiveCommerceCommentPayload(environment string) LiveCommerceCommentPayload {
	return LiveCommerceCommentPayload{EventType: eventTypeLiveCommerceComment, Environment: environment}
}

package exporter

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/logger"
)

// enrichQueries is the slice of *sqlc.Queries the exporter needs to fill in
// business fields the event payload doesn't carry (checkout_duration_ms,
// discount_cents, shipping_cents, the per-product breakdown, and the billing
// plan). Declared on the consumer side (same Go idiom as eventSender /
// CartPaymentGateway elsewhere in this codebase) so tests can inject a fake
// without a real database.
type enrichQueries interface {
	GetCartByID(ctx context.Context, id pgtype.UUID) (sqlc.Cart, error)
	ListCartItems(ctx context.Context, cartID pgtype.UUID) ([]sqlc.ListCartItemsRow, error)
	GetLedgerSaleEntry(ctx context.Context, cartID pgtype.UUID) (sqlc.BillingLedgerEntry, error)
	// FindCommentCartCorrelation is the comment↔cart join Fatia 4 needs for
	// OnCommentReceived (converted_to_cart). Declared here (not a separate
	// interface) so OnCommentReceived reuses the same nil-safe
	// Enricher/SetEnricher wiring as every other enrichment call site.
	FindCommentCartCorrelation(ctx context.Context, platformCommentID string) (sqlc.FindCommentCartCorrelationRow, error)
}

// Enricher runs the lightweight Postgres joins Fatia 3 needs to fill in the
// business fields Fatia 2 left at zero-value. It is entirely best-effort: any
// query failure is logged and swallowed — see Listeners' doc, telemetry must
// never propagate an error that could affect the outbox/asynq pipeline it
// piggybacks on, and enrichment matters even less than the base export.
type Enricher struct {
	queries enrichQueries
	logger  *zap.Logger
}

// NewEnricher builds an Enricher against the given queries handle.
func NewEnricher(queries enrichQueries, log *zap.Logger) *Enricher {
	return &Enricher{queries: queries, logger: log}
}

// CartSnapshot is the subset of a paid cart's row the exporter enriches
// LiveCommerceCart with: DiscountCents/ShippingCents already live on carts
// (coupon_discount_cents / shipping_cost_cents — set during checkout, stable
// by the time cart.paid fires, so no race with the fan-out reactors that run
// concurrently with this async dispatch). CheckoutDurationMs is derived from
// carts.initial_snapshot_taken_at -> carts.paid_at.
//
// KNOWN_LIMITATION: the Architect's original design used cart.checkout_armed's
// OccurredAt as the duration anchor, but that fact's only real producer today
// is RegenerateCheckout (an expired-checkout-link edge case, see
// order.Repository.RegenerateCheckout) — it does NOT fire on the normal first
// checkout visit, so keying off it would silently under-report duration for
// the common path. carts.initial_snapshot_taken_at ("the moment the buyer
// first opened the public checkout page", set once and immutable — see
// migration 000059) is the timestamp that actually covers every paid cart, so
// it is used instead. CheckoutDurationMs stays 0 (omitted, via
// json:",omitempty") for carts that never took that snapshot (e.g. paid
// through a path that skips the public checkout view).
type CartSnapshot struct {
	DiscountCents      int64
	ShippingCents      int64
	CheckoutDurationMs int64
}

// CartSnapshot fetches the enrichment fields for a paid cart. ok=false means
// the query failed (logged) or the cart id didn't parse — callers should skip
// enrichment and export the base payload rather than block or retry.
func (e *Enricher) CartSnapshot(ctx context.Context, cartID string) (CartSnapshot, bool) {
	id, err := idempotency.ParseUUID(cartID)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: invalid cart_id, skipping cart snapshot",
			zap.String("cart_id", cartID), zap.Error(err))
		return CartSnapshot{}, false
	}

	cart, err := e.queries.GetCartByID(ctx, id)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: GetCartByID failed, skipping cart snapshot",
			zap.String("cart_id", cartID), zap.Error(err))
		return CartSnapshot{}, false
	}

	snap := CartSnapshot{
		DiscountCents: cart.CouponDiscountCents,
		ShippingCents: cart.ShippingCostCents.Int64,
	}
	if cart.InitialSnapshotTakenAt.Valid && cart.PaidAt.Valid {
		if d := cart.PaidAt.Time.Sub(cart.InitialSnapshotTakenAt.Time); d > 0 {
			snap.CheckoutDurationMs = d.Milliseconds()
		}
	}
	return snap, true
}

// CartItemBreakdown returns one LiveCommerceCartItemPayload per line item of
// the cart, sourced from cart_items (not order_items): cart_items already
// exists at cart.paid time regardless of whether the immutable Order has been
// materialised yet by the concurrent orderListener.OnCartPaid reactor — this
// dispatch runs detached/async (dispatchTelemetryAsync), so it must not race
// order_items into existence. quantity*unit_price per row is the same formula
// as the canonical cart_product_total_cents (migration 000093), so the rows
// sum to the cart's GMVCents.
func (e *Enricher) CartItemBreakdown(ctx context.Context, cartID, liveEventID, storeID, environment string, timestampMs int64) []LiveCommerceCartItemPayload {
	id, err := idempotency.ParseUUID(cartID)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: invalid cart_id, skipping item breakdown",
			zap.String("cart_id", cartID), zap.Error(err))
		return nil
	}

	rows, err := e.queries.ListCartItems(ctx, id)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: ListCartItems failed, skipping item breakdown",
			zap.String("cart_id", cartID), zap.Error(err))
		return nil
	}

	items := make([]LiveCommerceCartItemPayload, 0, len(rows))
	for _, row := range rows {
		quantity := int64(row.Quantity.Int32)
		unitPrice := row.UnitPrice.Int64

		item := NewLiveCommerceCartItemPayload(environment)
		item.LiveEventID = liveEventID
		item.StoreID = storeID
		item.CartID = cartID
		item.ProductID = uuidString(row.ProductID)
		item.ProductName = row.ProductName
		item.Quantity = quantity
		item.UnitPriceCents = unitPrice
		item.RevenueCents = quantity * unitPrice
		item.Timestamp = timestampMs
		items = append(items, item)
	}
	return items
}

// LedgerInfo is the billing_ledger_entries fields the exporter needs for
// gmv.recorded/gmv.refunded that aren't already in the event payload (Plan —
// AmountCents/FeeCents/Billable ride the payload emitted by
// billing.Service.OnCartPaid/OnCartRefunded).
type LedgerInfo struct {
	Plan     string
	Billable bool
}

// LedgerPlan fetches the sale ledger entry's plan/billable for a cart. Reuses
// GetLedgerSaleEntry (already used by billing.Service.OnCartRefunded) instead
// of adding a new query — the 'sale' row persists after a refund (a separate
// 'refund_credit' row is inserted alongside it), so this resolves the plan for
// both gmv.recorded and gmv.refunded. ok=false on any failure (cart id parse,
// no sale row, DB error) — callers skip the Plan/Billable enrichment.
func (e *Enricher) LedgerPlan(ctx context.Context, cartID string) (LedgerInfo, bool) {
	id, err := idempotency.ParseUUID(cartID)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: invalid cart_id, skipping ledger lookup",
			zap.String("cart_id", cartID), zap.Error(err))
		return LedgerInfo{}, false
	}

	entry, err := e.queries.GetLedgerSaleEntry(ctx, id)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: GetLedgerSaleEntry failed, skipping ledger lookup",
			zap.String("cart_id", cartID), zap.Error(err))
		return LedgerInfo{}, false
	}

	return LedgerInfo{Plan: entry.Plan, Billable: entry.Billable}, true
}

// uuidString renders a pgtype.UUID as its canonical string form, or "" when
// unset — mirrors the uuidToString helpers already used by order/integration
// (kept local: the exporter package doesn't import those internal packages).
func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

// CommentCorrelation is the comment↔cart conversion signal OnCommentReceived
// exports as LiveCommerceCommentPayload. ConvertedToCart/CartID/TimeToCartMs
// come from FindCommentCartCorrelation's LEFT JOIN — CartID == "" means no
// cart_item_events row (cart item addition) matched the 120s window.
type CommentCorrelation struct {
	LiveEventID       string
	StoreID           string
	SessionID         string
	Platform          string
	PlatformUserID    string
	HasPurchaseIntent bool
	MatchedProductID  string
	Result            string
	ConvertedToCart   bool
	CartID            string
	TimeToCartMs      int64
}

// CommentCartCorrelation runs the comment↔cart correlation join for a comment
// identified by its platform_comment_id. ok=false means the comment isn't
// persisted yet (query failure or no matching row) — callers should skip the
// export rather than emit a payload with zero-value business fields.
func (e *Enricher) CommentCartCorrelation(ctx context.Context, platformCommentID string) (CommentCorrelation, bool) {
	row, err := e.queries.FindCommentCartCorrelation(ctx, platformCommentID)
	if err != nil {
		logger.From(ctx, e.logger).Warn("telemetry enrich: FindCommentCartCorrelation failed, skipping comment correlation",
			zap.String("platform_comment_id", platformCommentID), zap.Error(err))
		return CommentCorrelation{}, false
	}

	result := CommentCorrelation{
		LiveEventID:       uuidString(row.EventID),
		StoreID:           uuidString(row.StoreID),
		SessionID:         uuidString(row.SessionID),
		Platform:          row.Platform,
		PlatformUserID:    row.PlatformUserID,
		HasPurchaseIntent: row.HasPurchaseIntent.Bool,
		MatchedProductID:  uuidString(row.MatchedProductID),
		Result:            row.Result.String,
	}

	if row.CartID.Valid {
		result.ConvertedToCart = true
		result.CartID = uuidString(row.CartID)
		// TimeToCartMs measures comment -> item ADDED, not comment -> cart
		// CREATED: on a comment that reuses an already-open cart (buyer
		// commenting a 2nd time in the same event to add another product),
		// carts.created_at predates the comment by a wide margin, while
		// cart_item_events.created_at is the actual moment this comment's
		// item landed in the cart.
		if row.CommentCreatedAt.Valid && row.ItemCreatedAt.Valid {
			if d := row.ItemCreatedAt.Time.Sub(row.CommentCreatedAt.Time); d >= 0 {
				result.TimeToCartMs = d.Milliseconds()
			}
		}
	}

	return result, true
}

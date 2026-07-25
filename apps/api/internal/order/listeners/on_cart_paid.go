// Package listeners contains the Order module's event reactors.
// Each reactor subscribes to a domain fact (cart.paid, cart.expired, etc.)
// and is idempotent — safe for at-least-once delivery with asynq retry + DLQ.
package listeners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/logger"
)

// Listener reacts to domain facts and materialises the Order aggregate.
type Listener struct {
	pool    *pgxpool.Pool
	queries *sqlc.Queries
	logger  *zap.Logger
}

func New(pool *pgxpool.Pool, queries *sqlc.Queries, log *zap.Logger) *Listener {
	return &Listener{
		pool:    pool,
		queries: queries,
		logger:  log.Named("order.listener"),
	}
}

// EnsureOrderForCheckout creates the Order draft (status=pending_payment) at
// checkout initiation. Best-effort: errors are logged and swallowed — the
// checkout flow must never block waiting for this.
//
// Idempotent: if an Order already exists for the cart → no-op.
func (l *Listener) EnsureOrderForCheckout(ctx context.Context, cartID, storeID string) {
	if err := l.ensureOrderDraft(ctx, cartID, storeID); err != nil {
		logger.From(ctx, l.logger).Warn("EnsureOrderForCheckout failed (best-effort)",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
}

// ensureOrderDraft is the implementation behind EnsureOrderForCheckout.
func (l *Listener) ensureOrderDraft(ctx context.Context, cartID, storeID string) error {
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("invalid cart_id %q: %w", cartID, err)
	}

	log := logger.From(ctx, l.logger).With(zap.String("cart_id", cartID))

	// Idempotency: order already exists for this cart → no-op.
	if _, err := l.queries.GetOrderIDByCartID(ctx, cid); err == nil {
		log.Debug("EnsureOrderForCheckout: order already exists, skipping")
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("checking existing order: %w", err)
	}

	cart, err := l.queries.GetCartByID(ctx, cid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("cart %s not found", cartID)
		}
		return fmt.Errorf("loading cart: %w", err)
	}

	storeUUID, err := l.resolveStoreID(ctx, storeID, cart.EventID)
	if err != nil {
		return fmt.Errorf("resolving store_id: %w", err)
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := l.queries.WithTx(tx)

	// Draft order: identity only, totals and paid_at remain NULL.
	orderRow, err := qtx.InsertOrder(ctx, sqlc.InsertOrderParams{
		CartID:         cid,
		ShortID:        cart.ShortID,
		StoreID:        storeUUID,
		EventID:        cart.EventID,
		CustomerID:     cart.CustomerID,
		Status:         "pending_payment",
		TotalCents:     pgtype.Int8{Valid: false},
		DiscountCents:  0,
		ShippingCents:  0,
		PaidTotalCents: pgtype.Int8{Valid: false},
		PaidAt:         pgtype.Timestamptz{Valid: false},
	})
	if err != nil {
		return fmt.Errorf("insert draft order: %w", err)
	}

	// Draft payment row (1:1): payment_status=pending, all details NULL.
	if err := qtx.InsertOrderPayment(ctx, sqlc.InsertOrderPaymentParams{
		OrderID:             orderRow.ID,
		PaymentStatus:       "pending",
		PaymentMethod:       pgtype.Text{},
		CardSnapshot:        nil,
		GatewaySnapshot:     nil,
		CouponID:            pgtype.UUID{},
		CouponCode:          pgtype.Text{},
		CouponDiscountCents: 0,
	}); err != nil {
		return fmt.Errorf("insert draft order_payment: %w", err)
	}

	// Draft logistics row (1:1): all fields NULL — snapshot sealed at paid.
	if err := qtx.InsertOrderLogistics(ctx, sqlc.InsertOrderLogisticsParams{
		OrderID:               orderRow.ID,
		ShippingAddress:       nil,
		ShippingServiceID:     pgtype.Int4{},
		ShippingServiceName:   pgtype.Text{},
		ShippingCarrier:       pgtype.Text{},
		ShippingCostCents:     pgtype.Int8{},
		ShippingCostRealCents: pgtype.Int8{},
		ShippingDeadlineDays:  pgtype.Int4{},
		TrackingToken:         pgtype.Text{},
	}); err != nil {
		return fmt.Errorf("insert draft order_logistics: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit draft: %w", err)
	}

	log.Info("EnsureOrderForCheckout: draft order created",
		zap.String("order_id", uuidStr(orderRow.ID)),
	)
	return nil
}

// OnCartPaid materialises (or seals) the Order from the paid cart snapshot.
//
// Decision tree (idempotent throughout):
//   - Draft exists (pending_payment) → seal snapshot + transition to paid.
//   - Order already paid → no-op (idempotent delivery).
//   - No order at all → create on-the-fly already in paid (Fatia 1 fallback).
//
// storeID may be empty (backfill path); in that case it is resolved from live_events.
func (l *Listener) OnCartPaid(ctx context.Context, cartID, storeID string, gmvCents int64, paymentSnapshot json.RawMessage) error {
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: invalid cart_id %q: %w", cartID, err)
	}

	log := logger.From(ctx, l.logger).With(zap.String("cart_id", cartID))

	// Check for existing order and its status.
	existing, err := l.queries.GetOrderByCartIDForSeal(ctx, cid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("order OnCartPaid: checking existing order: %w", err)
	}

	if err == nil {
		// Order exists.
		if existing.Status == "paid" {
			log.Debug("OnCartPaid: order already paid, skipping")
			return nil
		}
		// Draft exists (pending_payment) → seal it.
		return l.sealDraftOrder(ctx, existing.ID, cid, storeID, gmvCents, paymentSnapshot, log)
	}

	// No order yet → create on-the-fly already in paid (best-effort rollout fallback).
	return l.createPaidOrder(ctx, cid, storeID, gmvCents, paymentSnapshot, log)
}

// sealDraftOrder transitions an existing pending_payment Order to paid by
// loading the cart snapshot and writing all deferred fields in one transaction.
func (l *Listener) sealDraftOrder(
	ctx context.Context,
	orderID pgtype.UUID,
	cartID pgtype.UUID,
	storeID string,
	gmvCents int64,
	paymentSnapshot json.RawMessage,
	log *zap.Logger,
) error {
	cart, err := l.queries.GetCartByID(ctx, cartID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("order OnCartPaid: cart %s not found", uuidStr(cartID))
		}
		return fmt.Errorf("order OnCartPaid: loading cart: %w", err)
	}

	storeUUID, err := l.resolveStoreID(ctx, storeID, cart.EventID)
	_ = storeUUID // resolved for logging consistency; order already has store_id

	if gmvCents <= 0 {
		gmvCents, err = l.queries.GetCartGMVCents(ctx, cartID)
		if err != nil {
			log.Warn("OnCartPaid: fallback GetCartGMVCents failed", zap.Error(err))
			gmvCents = 0
		}
	}

	discountCents := cart.CouponDiscountCents
	shippingCents := int64(0)
	if cart.ShippingCostCents.Valid {
		shippingCents = cart.ShippingCostCents.Int64
	}
	paidTotal := gmvCents - discountCents + shippingCents

	items, err := l.queries.GetCartItemsForOrderMaterialization(ctx, cartID)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: loading cart items: %w", err)
	}

	cardSnap, _ := buildCardSnapshot(cart)

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := l.queries.WithTx(tx)

	// Seal the order root with totals.
	if err := qtx.SealOrder(ctx, sqlc.SealOrderParams{
		ID:             orderID,
		TotalCents:     pgtype.Int8{Int64: gmvCents, Valid: true},
		DiscountCents:  discountCents,
		ShippingCents:  shippingCents,
		PaidTotalCents: pgtype.Int8{Int64: paidTotal, Valid: true},
		PaidAt:         cart.PaidAt,
	}); err != nil {
		return fmt.Errorf("order OnCartPaid: seal order: %w", err)
	}

	// Insert immutable item snapshot.
	for _, item := range items {
		if err := qtx.InsertOrderItem(ctx, sqlc.InsertOrderItemParams{
			OrderID:     orderID,
			ProductID:   item.ProductID,
			ProductName: item.ProductName,
			Quantity:    item.Quantity,
			UnitPrice:   item.UnitPrice,
		}); err != nil {
			return fmt.Errorf("order OnCartPaid: insert order_item product=%s: %w",
				uuidStr(item.ProductID), err)
		}
	}

	// Seal payment details.
	if err := qtx.SealOrderPayment(ctx, sqlc.SealOrderPaymentParams{
		OrderID:             orderID,
		PaymentMethod:       cart.PaymentMethod,
		CardSnapshot:        cardSnap,
		GatewaySnapshot:     paymentSnapshot,
		CouponID:            cart.CouponID,
		CouponCode:          cart.CouponCode,
		CouponDiscountCents: discountCents,
	}); err != nil {
		return fmt.Errorf("order OnCartPaid: seal order_payment: %w", err)
	}

	// Seal logistics snapshot.
	logParams := sqlc.SealOrderLogisticsParams{
		OrderID:               orderID,
		ShippingAddress:       cart.ShippingAddress,
		ShippingServiceName:   cart.ShippingServiceName,
		ShippingCarrier:       cart.ShippingCarrier,
		ShippingCostCents:     cart.ShippingCostCents,
		ShippingCostRealCents: cart.ShippingCostRealCents,
		ShippingDeadlineDays:  pgtype.Int4{Int32: cart.ShippingDeadlineDays.Int32, Valid: cart.ShippingDeadlineDays.Valid},
		TrackingToken:         cart.TrackingToken,
	}
	if cart.ShippingServiceID.Valid && cart.ShippingServiceID.String != "" {
		if n, parseErr := strconv.ParseInt(cart.ShippingServiceID.String, 10, 32); parseErr == nil {
			logParams.ShippingServiceID = pgtype.Int4{Int32: int32(n), Valid: true}
		}
	}
	if err := qtx.SealOrderLogistics(ctx, logParams); err != nil {
		return fmt.Errorf("order OnCartPaid: seal order_logistics: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("order OnCartPaid: commit seal: %w", err)
	}

	log.Info("OnCartPaid: draft sealed → paid",
		zap.String("order_id", uuidStr(orderID)),
		zap.Int64("total_cents", gmvCents),
		zap.Int64("paid_total_cents", paidTotal),
	)
	return nil
}

// createPaidOrder inserts a brand-new Order already in paid status (no prior
// draft). This is the Fatia 1 fallback: runs when EnsureOrderForCheckout was
// never called (old carts, backfill) or when the best-effort draft was lost.
func (l *Listener) createPaidOrder(
	ctx context.Context,
	cid pgtype.UUID,
	storeID string,
	gmvCents int64,
	paymentSnapshot json.RawMessage,
	log *zap.Logger,
) error {
	cart, err := l.queries.GetCartByID(ctx, cid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("order OnCartPaid: cart %s not found", uuidStr(cid))
		}
		return fmt.Errorf("order OnCartPaid: loading cart: %w", err)
	}

	storeUUID, err := l.resolveStoreID(ctx, storeID, cart.EventID)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: resolving store_id: %w", err)
	}

	if gmvCents <= 0 {
		gmvCents, err = l.queries.GetCartGMVCents(ctx, cid)
		if err != nil {
			log.Warn("OnCartPaid: fallback GetCartGMVCents failed", zap.Error(err))
			gmvCents = 0
		}
	}

	discountCents := cart.CouponDiscountCents
	shippingCents := int64(0)
	if cart.ShippingCostCents.Valid {
		shippingCents = cart.ShippingCostCents.Int64
	}
	paidTotal := gmvCents - discountCents + shippingCents

	items, err := l.queries.GetCartItemsForOrderMaterialization(ctx, cid)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: loading cart items: %w", err)
	}

	cardSnap, _ := buildCardSnapshot(cart)

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := l.queries.WithTx(tx)

	orderRow, err := qtx.InsertOrder(ctx, sqlc.InsertOrderParams{
		CartID:         cid,
		ShortID:        cart.ShortID,
		StoreID:        storeUUID,
		EventID:        cart.EventID,
		CustomerID:     cart.CustomerID,
		Status:         "paid",
		TotalCents:     pgtype.Int8{Int64: gmvCents, Valid: true},
		DiscountCents:  discountCents,
		ShippingCents:  shippingCents,
		PaidTotalCents: pgtype.Int8{Int64: paidTotal, Valid: true},
		PaidAt:         cart.PaidAt,
	})
	if err != nil {
		return fmt.Errorf("order OnCartPaid: insert order: %w", err)
	}

	for _, item := range items {
		if err := qtx.InsertOrderItem(ctx, sqlc.InsertOrderItemParams{
			OrderID:     orderRow.ID,
			ProductID:   item.ProductID,
			ProductName: item.ProductName,
			Quantity:    item.Quantity,
			UnitPrice:   item.UnitPrice,
		}); err != nil {
			return fmt.Errorf("order OnCartPaid: insert order_item product=%s: %w",
				uuidStr(item.ProductID), err)
		}
	}

	if err := qtx.InsertOrderPayment(ctx, sqlc.InsertOrderPaymentParams{
		OrderID:             orderRow.ID,
		PaymentStatus:       "paid",
		PaymentMethod:       cart.PaymentMethod,
		CardSnapshot:        cardSnap,
		GatewaySnapshot:     paymentSnapshot,
		CouponID:            cart.CouponID,
		CouponCode:          cart.CouponCode,
		CouponDiscountCents: discountCents,
	}); err != nil {
		return fmt.Errorf("order OnCartPaid: insert order_payment: %w", err)
	}

	logParams := sqlc.InsertOrderLogisticsParams{
		OrderID:               orderRow.ID,
		ShippingAddress:       cart.ShippingAddress,
		ShippingServiceName:   cart.ShippingServiceName,
		ShippingCarrier:       cart.ShippingCarrier,
		ShippingCostCents:     cart.ShippingCostCents,
		ShippingCostRealCents: cart.ShippingCostRealCents,
		ShippingDeadlineDays:  pgtype.Int4{Int32: cart.ShippingDeadlineDays.Int32, Valid: cart.ShippingDeadlineDays.Valid},
		TrackingToken:         cart.TrackingToken,
	}
	if cart.ShippingServiceID.Valid && cart.ShippingServiceID.String != "" {
		if n, parseErr := strconv.ParseInt(cart.ShippingServiceID.String, 10, 32); parseErr == nil {
			logParams.ShippingServiceID = pgtype.Int4{Int32: int32(n), Valid: true}
		}
	}

	if err := qtx.InsertOrderLogistics(ctx, logParams); err != nil {
		return fmt.Errorf("order OnCartPaid: insert order_logistics: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("order OnCartPaid: commit: %w", err)
	}

	log.Info("OnCartPaid: order materialised (on-the-fly)",
		zap.String("order_id", uuidStr(orderRow.ID)),
		zap.Int64("total_cents", gmvCents),
		zap.Int64("paid_total_cents", paidTotal),
	)
	return nil
}

// BackfillFromPaidCarts materialises Orders for all paid carts that don't yet
// have one. Uses the same OnCartPaid path — no divergent logic.
func (l *Listener) BackfillFromPaidCarts(ctx context.Context) (int, error) {
	cartUUIDs, err := l.queries.GetPaidCartsWithoutOrder(ctx)
	if err != nil {
		return 0, fmt.Errorf("order backfill: listing carts: %w", err)
	}

	count := 0
	for _, cid := range cartUUIDs {
		cartID := uuidStr(cid)
		// storeID="" → listener resolves from live_events.
		// gmvCents=0 → fallback to GetCartGMVCents.
		// paymentSnapshot=nil → stored as NULL (historical carts, no gateway snapshot).
		if err := l.OnCartPaid(ctx, cartID, "", 0, nil); err != nil {
			l.logger.Warn("backfill: failed to materialise order",
				zap.String("cart_id", cartID),
				zap.Error(err),
			)
			continue
		}
		count++
	}
	return count, nil
}

// resolveStoreID returns the store UUID. Uses the provided storeID string when
// non-empty; otherwise falls back to querying live_events by eventID.
func (l *Listener) resolveStoreID(ctx context.Context, storeID string, eventID pgtype.UUID) (pgtype.UUID, error) {
	if storeID != "" {
		return parseUUID(storeID)
	}
	var sid pgtype.UUID
	if err := l.pool.QueryRow(ctx,
		`SELECT store_id FROM live_events WHERE id = $1`, eventID,
	).Scan(&sid); err != nil {
		return pgtype.UUID{}, fmt.Errorf("live_events lookup for event %v: %w", eventID, err)
	}
	return sid, nil
}

// buildCardSnapshot encodes the cart's card_* columns into a JSONB snapshot.
// Returns nil when no card data is present (PIX payments, etc.).
func buildCardSnapshot(cart sqlc.Cart) (json.RawMessage, error) {
	if !cart.CardBrand.Valid && !cart.CardLastFour.Valid {
		return nil, nil
	}
	snap := map[string]any{
		"brand":              cart.CardBrand.String,
		"last_four":          cart.CardLastFour.String,
		"installments":       cart.CardInstallments.Int32,
		"authorization_code": cart.CardAuthorizationCode.String,
	}
	return json.Marshal(snap)
}

func parseUUID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

func uuidStr(u pgtype.UUID) string {
	return uuid.UUID(u.Bytes).String()
}

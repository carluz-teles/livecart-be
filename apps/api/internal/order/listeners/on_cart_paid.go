// Package listeners contains the Order module's event reactors.
// Each reactor subscribes to a domain fact (cart.paid, etc.)
// and is idempotent — safe for at-least-once delivery with asynq retry + DLQ.
package listeners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
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

// OnCartPaid materialises the Order from the paid cart snapshot in paid status.
// Order nasce já em paid — não há draft precedente.
//
// Idempotent: if an Order already exists for the cart → no-op.
// storeID may be empty (backfill path); in that case it is resolved from live_events.
func (l *Listener) OnCartPaid(ctx context.Context, cartID, storeID string, gmvCents int64, paymentSnapshot json.RawMessage) error {
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: invalid cart_id %q: %w", cartID, err)
	}

	log := logger.From(ctx, l.logger).With(zap.String("cart_id", cartID))

	// Idempotency: Order already exists → it is already paid, skip.
	if _, err := l.queries.GetOrderIDByCartID(ctx, cid); err == nil {
		log.Debug("OnCartPaid: order already exists, skipping")
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("order OnCartPaid: checking existing order: %w", err)
	}

	if err := l.createPaidOrder(ctx, cid, storeID, gmvCents, paymentSnapshot, log); err != nil {
		return err
	}
	// Espelho pós-pagamento: projeta o snapshot ERP do cart na Order. Best-effort.
	l.MirrorCartERPToOrder(ctx, cartID)
	return nil
}

// createPaidOrder inserts a brand-new Order already in paid status.
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
	customerSnap, _ := buildCustomerSnapshot(cart)

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("order OnCartPaid: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := l.queries.WithTx(tx)

	orderRow, err := qtx.InsertOrder(ctx, sqlc.InsertOrderParams{
		CartID:           cid,
		ShortID:          cart.ShortID,
		StoreID:          storeUUID,
		EventID:          cart.EventID,
		CustomerID:       cart.CustomerID,
		Status:           "paid",
		TotalCents:       pgtype.Int8{Int64: gmvCents, Valid: true},
		DiscountCents:    discountCents,
		ShippingCents:    shippingCents,
		PaidTotalCents:   pgtype.Int8{Int64: paidTotal, Valid: true},
		PaidAt:           cart.PaidAt,
		CustomerSnapshot: customerSnap,
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

	// shipping_service_id é um id opaco (int-as-string p/ ME, ObjectId/UUID p/
	// outros) — congelado como TEXT, igual à fonte carts, sem parse com perda.
	//
	// tracking_token nasce AQUI (Fatia A4): a Order é o dono canônico do rastreio
	// (order_logistics é a fonte da verdade — Fatia 10-a), então o token é gerado
	// na própria materialização, atômico com a Order. O postcheckout não o gera
	// mais. Como esta materialização só roda quando a Order ainda não existe (early
	// return em OnCartPaid), o token é set-once por construção.
	trackingToken, err := generateTrackingToken()
	if err != nil {
		return fmt.Errorf("order OnCartPaid: generate tracking token: %w", err)
	}
	logParams := sqlc.InsertOrderLogisticsParams{
		OrderID:               orderRow.ID,
		ShippingAddress:       cart.ShippingAddress,
		ShippingProvider:      cart.ShippingProvider,
		ShippingServiceID:     cart.ShippingServiceID,
		ShippingServiceName:   cart.ShippingServiceName,
		ShippingCarrier:       cart.ShippingCarrier,
		ShippingCostCents:     cart.ShippingCostCents,
		ShippingCostRealCents: cart.ShippingCostRealCents,
		ShippingDeadlineDays:  pgtype.Int4{Int32: cart.ShippingDeadlineDays.Int32, Valid: cart.ShippingDeadlineDays.Valid},
		TrackingToken:         pgtype.Text{String: trackingToken, Valid: true},
	}

	if err := qtx.InsertOrderLogistics(ctx, logParams); err != nil {
		return fmt.Errorf("order OnCartPaid: insert order_logistics: %w", err)
	}

	// Timeline `payment_confirmed` nasce na MESMA tx da Order (Fatia A4): token +
	// timeline são atômicos com a materialização. Idempotente via ON CONFLICT
	// (order_id, event_type) DO NOTHING — o insert :one devolve pgx.ErrNoRows
	// quando a linha já existe, que aqui é o caminho de dedup, não um erro.
	if _, err := qtx.InsertOrderEvent(ctx, sqlc.InsertOrderEventParams{
		OrderID:    orderRow.ID,
		CartID:     cid,
		EventType:  "payment_confirmed",
		OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Source:     "system",
		Metadata:   json.RawMessage("{}"),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("order OnCartPaid: insert payment_confirmed event: %w", err)
	}

	// Fatia 11a: emit the canonical order.paid bus fact in the SAME tx as the
	// materialisation (via the outbox) → exactly-once, never lost between commit
	// and enqueue. No consumer yet — 11b wires the ERP finalisation reactor onto
	// it. Dedup by order_id so an at-least-once redelivery of cart.paid collapses.
	orderID := uuidStr(orderRow.ID)
	orderPaid := struct {
		OrderID         string          `json:"order_id"`
		CartID          string          `json:"cart_id"`
		StoreID         string          `json:"store_id"`
		GMVCents        int64           `json:"gmv_cents"`
		PaymentSnapshot json.RawMessage `json:"payment_snapshot,omitempty"`
	}{orderID, uuidStr(cid), uuidStr(storeUUID), gmvCents, paymentSnapshot}
	if err := events.EmitInternal(ctx, qtx, events.OrderPaid, "order.paid:"+orderID, orderPaid); err != nil {
		return fmt.Errorf("order OnCartPaid: emit order.paid: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("order OnCartPaid: commit: %w", err)
	}

	log.Info("OnCartPaid: order materialised",
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

// buildCustomerSnapshot freezes the cart's customer_* columns into an immutable
// JSONB carried on the Order, so the order detail no longer depends on carts.
// Always returns an object (never nil) — the order detail reads it by key.
func buildCustomerSnapshot(cart sqlc.Cart) (json.RawMessage, error) {
	snap := map[string]string{
		"name":     cart.CustomerName.String,
		"email":    cart.CustomerEmail.String,
		"document": cart.CustomerDocument.String,
		"phone":    cart.CustomerPhone.String,
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

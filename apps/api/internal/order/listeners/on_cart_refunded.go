package listeners

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

// Terminal statuses the immutable Order can move into after being paid.
const (
	orderStatusPaid      = "paid"
	orderStatusRefunded  = "refunded"
	orderStatusCancelled = "cancelled"
)

// Sentinels so the composition-root wiring can tell a benign missing-Order (an
// unpaid cart being cancelled) from a genuine bad state transition.
var (
	// ErrOrderNotMaterialised: no Order row exists for the cart. For cart.refunded
	// this means the cart.paid materialisation is still in flight (retryable); for
	// cart.cancelled of an unpaid cart it is expected and benign (wiring skips it).
	ErrOrderNotMaterialised = errors.New("order not materialised for cart")
	// ErrInvalidOrderTransition: the Order exists but is not in 'paid' status, so it
	// cannot move to the requested terminal status (refunded/cancelled).
	ErrInvalidOrderTransition = errors.New("invalid order status transition")
)

// OnCartRefunded reacts to cart.refunded: it flips the Order — and its payment
// row — to 'refunded'. Idempotent: a redelivery on an already-refunded Order is a
// no-op. storeID is used only for log correlation (the Order is keyed by cart_id)
// and may be empty.
func (l *Listener) OnCartRefunded(ctx context.Context, cartID, storeID string) error {
	return l.applyTerminalStatus(ctx, cartID, storeID, orderStatusRefunded)
}

// applyTerminalStatus moves the Order for cartID into target ('refunded' /
// 'cancelled'), mirroring it onto order_payments.payment_status, in one tx.
//
// Guards (no mutation on any of these):
//   - no Order for the cart          → ErrOrderNotMaterialised
//   - Order already in target status → no-op, nil (idempotent, at-least-once safe)
//   - Order not in 'paid' status     → ErrInvalidOrderTransition
//
// Error paths return the (wrapped) error unlogged: the asynq server surfaces it
// via the project logger at the transport boundary (single handling rule). The
// success and idempotent-skip paths — which do not return an error — log here.
func (l *Listener) applyTerminalStatus(ctx context.Context, cartID, storeID, target string) error {
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("order applyTerminalStatus: invalid cart_id %q: %w", cartID, err)
	}

	ctx = logger.WithStore(ctx, storeID, "")
	log := logger.From(ctx, l.logger).With(
		zap.String("cart_id", cartID),
		zap.String("target_status", target),
	)

	row, err := l.queries.GetOrderStatusByCartID(ctx, cid)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("order applyTerminalStatus: cart %s: %w", cartID, ErrOrderNotMaterialised)
	}
	if err != nil {
		return fmt.Errorf("order applyTerminalStatus: loading order status: %w", err)
	}

	// Idempotent: already in the terminal target → nothing to do.
	if row.Status == target {
		log.Debug("terminal transition: order already in target status, skipping")
		return nil
	}

	// Only a paid Order may move to a terminal refunded/cancelled status.
	if row.Status != orderStatusPaid {
		return fmt.Errorf("order applyTerminalStatus: cart %s order in status %q, want %q: %w",
			cartID, row.Status, orderStatusPaid, ErrInvalidOrderTransition)
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("order applyTerminalStatus: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := l.queries.WithTx(tx)

	if err := qtx.SetOrderStatusByCartID(ctx, sqlc.SetOrderStatusByCartIDParams{
		CartID: cid,
		Status: target,
	}); err != nil {
		return fmt.Errorf("order applyTerminalStatus: update orders.status: %w", err)
	}

	if err := qtx.SetOrderPaymentStatusByCartID(ctx, sqlc.SetOrderPaymentStatusByCartIDParams{
		CartID:        cid,
		PaymentStatus: target,
	}); err != nil {
		return fmt.Errorf("order applyTerminalStatus: update order_payments.payment_status: %w", err)
	}

	// Fatia 11a: emit the canonical order.refunded bus fact in the SAME tx as the
	// flip (via the outbox) → exactly-once. Only for the refunded transition —
	// there is no order.cancelled bus fact in scope (this method is shared with
	// OnCartCancelled). No consumer yet; 11b wires the ERP refund reactor. Dedup
	// by order_id collapses an at-least-once redelivery of cart.refunded.
	if target == orderStatusRefunded {
		orderID := uuidStr(row.ID)
		orderRefunded := struct {
			OrderID string `json:"order_id"`
			CartID  string `json:"cart_id"`
			StoreID string `json:"store_id"`
		}{orderID, cartID, storeID}
		if err := events.EmitInternal(ctx, qtx, events.OrderRefunded, "order.refunded:"+orderID, orderRefunded); err != nil {
			return fmt.Errorf("order applyTerminalStatus: emit order.refunded: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("order applyTerminalStatus: commit: %w", err)
	}

	log.Info("order moved to terminal status",
		zap.String("order_id", uuidStr(row.ID)),
		zap.String("previous_status", orderStatusPaid),
	)
	return nil
}

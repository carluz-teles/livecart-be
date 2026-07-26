package listeners

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// MirrorCartERPToOrder projects the ERP state from the cart into the Order
// aggregate (order_logistics + order_payments). Best-effort: errors are logged
// and swallowed — a mirror failure NEVER breaks the ERP flow.
func (l *Listener) MirrorCartERPToOrder(ctx context.Context, cartID string) {
	if err := l.mirrorCartERPToOrder(ctx, cartID); err != nil {
		logger.From(ctx, l.logger).Warn("MirrorCartERPToOrder failed (best-effort)",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
}

// mirrorCartERPToOrder is the implementation of MirrorCartERPToOrder.
//
// Fatia 11b: only the RESERVE state is projected from the cart — the finalisation
// lifecycle (erp_finalisation_*) and the NFe (invoice_*) are now written directly
// to order_payments by the ERP reactors, so mirroring them here would clobber the
// authoritative rows. What stays: erp_order_state/erp_stock_launched/
// erp_op_started_at → order_logistics, and external_order_id → order_payments
// (external_order_id remains authoritative on the cart, created at reservation).
// If no Order exists for the cart (best-effort rollout), returns nil untouched.
func (l *Listener) mirrorCartERPToOrder(ctx context.Context, cartID string) error {
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("invalid cart_id: %w", err)
	}

	// No Order for this cart → no-op (best-effort; draft may not exist yet).
	orderID, err := l.queries.GetOrderIDByCartID(ctx, cid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking order for cart: %w", err)
	}

	// Read the reserve state (canonical query) — do NOT reimplement.
	erpState, err := l.queries.GetCartERPOrderState(ctx, cid)
	if err != nil {
		return fmt.Errorf("reading cart ERP order state: %w", err)
	}

	// erp_op_started_at is not covered by GetCartERPOrderState.
	var erpOpStartedAt pgtype.Timestamptz
	if err := l.pool.QueryRow(ctx,
		`SELECT erp_op_started_at FROM carts WHERE id = $1`, cid,
	).Scan(&erpOpStartedAt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reading erp_op_started_at: %w", err)
	}

	// Mirror → order_logistics (erp_order_state, erp_stock_launched, erp_op_started_at).
	if _, err := l.pool.Exec(ctx, `
		UPDATE order_logistics
		SET erp_order_state    = $2,
		    erp_stock_launched = $3,
		    erp_op_started_at  = $4
		WHERE order_id = $1
	`, orderID, erpState.ErpOrderState, erpState.ErpStockLaunched, erpOpStartedAt); err != nil {
		return fmt.Errorf("updating order_logistics ERP state: %w", err)
	}

	// Mirror → order_payments (external_order_id only — reserve column). The
	// finalisation/invoice columns are written authoritatively by the reactors.
	if _, err := l.pool.Exec(ctx, `
		UPDATE order_payments
		SET external_order_id = $2
		WHERE order_id = $1
	`, orderID, erpState.ExternalOrderID); err != nil {
		return fmt.Errorf("updating order_payments external_order_id: %w", err)
	}

	logger.From(ctx, l.logger).Debug("ERP reserve state mirrored to Order",
		zap.String("cart_id", cartID),
		zap.String("order_id", uuidStr(orderID)),
		zap.String("erp_order_state", erpState.ErpOrderState),
	)
	return nil
}

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
// Reads the ERP state from the cart using the canonical SQLC queries
// (GetCartERPOrderState, GetCartERPFinalisationStatus, GetCartERPInvoice) and
// writes it into order_logistics / order_payments via plain UPDATE. If no Order
// exists for the cart (best-effort rollout), returns nil without touching anything.
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

	// Read ERP state using the three canonical queries — do NOT reimplement.
	erpState, err := l.queries.GetCartERPOrderState(ctx, cid)
	if err != nil {
		return fmt.Errorf("reading cart ERP order state: %w", err)
	}

	erpFinalisation, err := l.queries.GetCartERPFinalisationStatus(ctx, cid)
	if err != nil {
		return fmt.Errorf("reading cart ERP finalisation: %w", err)
	}

	erpInvoice, err := l.queries.GetCartERPInvoice(ctx, cid)
	if err != nil {
		return fmt.Errorf("reading cart ERP invoice: %w", err)
	}

	// erp_op_started_at is not covered by the three canonical queries above.
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

	// Mirror → order_payments (external_order_id, erp_finalisation_*, invoice_*).
	if _, err := l.pool.Exec(ctx, `
		UPDATE order_payments
		SET external_order_id       = $2,
		    erp_finalisation_status = $3,
		    erp_last_error          = $4,
		    erp_last_attempt_at     = $5,
		    erp_attempts_count      = $6,
		    invoice_id              = $7,
		    invoice_key             = $8,
		    invoice_status          = $9,
		    invoice_emitted_at      = $10
		WHERE order_id = $1
	`,
		orderID,
		erpFinalisation.ExternalOrderID,
		erpFinalisation.ErpFinalisationStatus,
		erpFinalisation.ErpLastError,
		erpFinalisation.ErpLastAttemptAt,
		erpFinalisation.ErpAttemptsCount,
		erpInvoice.ErpInvoiceID,
		erpInvoice.ErpInvoiceKey,
		erpInvoice.ErpInvoiceStatus,
		erpInvoice.ErpInvoiceEmittedAt,
	); err != nil {
		return fmt.Errorf("updating order_payments ERP state: %w", err)
	}

	logger.From(ctx, l.logger).Debug("ERP state mirrored to Order",
		zap.String("cart_id", cartID),
		zap.String("order_id", uuidStr(orderID)),
		zap.String("erp_order_state", erpState.ErpOrderState),
		zap.String("erp_finalisation_status", erpFinalisation.ErpFinalisationStatus),
	)
	return nil
}

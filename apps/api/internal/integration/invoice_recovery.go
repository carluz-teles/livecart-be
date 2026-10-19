package integration

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
)

// UpsertCartERPInvoice retains the ERP observation and applies it if the Order
// exists. Neither a delayed webhook nor a process restart can lose the invoice
// merely because cart.paid is still being handled by another worker.
func (r *Repository) UpsertCartERPInvoice(ctx context.Context, p UpsertCartERPInvoiceParams) (int64, error) {
	if p.ObservedAt.IsZero() || p.InvoiceID == "" || p.ExternalOrderID == "" {
		return 0, fmt.Errorf("invoice observation requires a timestamp and ERP binding")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("starting invoice observation: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	// Serialize with cart rebinding and other invoice observations. Validate the
	// integration after the cart lock, including disabled/replaced integrations.
	var id pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM carts
 WHERE id=$1 AND store_id=$2 AND external_order_id=$3 FOR UPDATE`,
		p.CartID, p.StoreID, p.ExternalOrderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("invoice cart binding changed: %w", erp.ErrCartBusy)
	}
	if err != nil {
		return 0, fmt.Errorf("locking invoice cart: %w", err)
	}
	var integrationID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM integrations
 WHERE id=$1 AND store_id=$2 AND type='erp' AND status='active' FOR SHARE`,
		p.IntegrationID, p.StoreID).Scan(&integrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("invoice integration changed: %w", erp.ErrCartBusy)
	}
	if err != nil {
		return 0, fmt.Errorf("locking invoice integration: %w", err)
	}
	result, err := tx.Exec(ctx, `INSERT INTO cart_invoice_observations
 (cart_id,store_id,integration_id,external_order_id,invoice_id,invoice_key,invoice_status,emitted_at,observed_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(cart_id) DO UPDATE SET store_id=EXCLUDED.store_id,integration_id=EXCLUDED.integration_id,
 external_order_id=EXCLUDED.external_order_id,invoice_id=EXCLUDED.invoice_id,
 invoice_key=EXCLUDED.invoice_key,invoice_status=EXCLUDED.invoice_status,
 emitted_at=EXCLUDED.emitted_at,observed_at=EXCLUDED.observed_at,applied_at=NULL
 WHERE cart_invoice_observations.observed_at<=EXCLUDED.observed_at`,
		p.CartID, p.StoreID, p.IntegrationID, p.ExternalOrderID,
		p.InvoiceID, p.InvoiceKey, p.InvoiceStatus, p.EmittedAt, p.ObservedAt)
	if err != nil {
		return 0, fmt.Errorf("retaining invoice observation: %w", err)
	}
	if result.RowsAffected() == 0 {
		return 0, fmt.Errorf("invoice observation superseded: %w", erp.ErrCartBusy)
	}
	var emitted pgtype.Timestamptz
	if p.EmittedAt != nil {
		emitted = pgtype.Timestamptz{Time: *p.EmittedAt, Valid: !p.EmittedAt.IsZero()}
	}
	rows, err := r.queries.WithTx(tx).UpsertOrderERPInvoice(ctx, sqlc.UpsertOrderERPInvoiceParams{
		CartID:        id,
		InvoiceID:     pgtype.Text{String: p.InvoiceID, Valid: true},
		InvoiceKey:    pgtype.Text{String: p.InvoiceKey, Valid: p.InvoiceKey != ""},
		InvoiceStatus: pgtype.Text{String: p.InvoiceStatus, Valid: p.InvoiceStatus != ""},
		EmittedAt:     emitted,
	})
	if err != nil {
		return 0, fmt.Errorf("applying retained invoice: %w", err)
	}
	if rows > 0 {
		if _, err := tx.Exec(ctx, `UPDATE cart_invoice_observations SET applied_at=now() WHERE cart_id=$1`, id); err != nil {
			return 0, fmt.Errorf("completing invoice observation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing invoice observation: %w", err)
	}
	return rows, nil
}

// RecoverCartInvoices only copies already observed invoice metadata onto an
// existing local Order. It never reads the ERP, creates orders or records money.
func (s *Service) RecoverCartInvoices(ctx context.Context) {
	rows, err := s.repo.pool.Query(ctx, `SELECT n.cart_id::text,n.store_id::text,n.integration_id::text,
 n.external_order_id,n.invoice_id,n.invoice_key,n.invoice_status,n.emitted_at,n.observed_at
 FROM cart_invoice_observations n
 JOIN carts c ON c.id=n.cart_id AND c.store_id=n.store_id AND c.external_order_id=n.external_order_id
 JOIN integrations i ON i.id=n.integration_id AND i.store_id=n.store_id AND i.type='erp' AND i.status='active'
 JOIN orders o ON o.cart_id=c.id AND o.store_id=c.store_id
 JOIN order_payments op ON op.order_id=o.id
 WHERE n.applied_at IS NULL ORDER BY n.observed_at,n.cart_id LIMIT 100`)
	if err != nil {
		s.logger.Error("listing retained invoices", zap.Error(err))
		return
	}
	batch := []UpsertCartERPInvoiceParams{}
	for rows.Next() {
		var p UpsertCartERPInvoiceParams
		if err = rows.Scan(&p.CartID, &p.StoreID, &p.IntegrationID, &p.ExternalOrderID,
			&p.InvoiceID, &p.InvoiceKey, &p.InvoiceStatus, &p.EmittedAt, &p.ObservedAt); err != nil {
			break
		}
		batch = append(batch, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.logger.Error("reading retained invoices", zap.Error(err))
		return
	}
	for _, p := range batch {
		if ctx.Err() != nil {
			return
		}
		count, err := s.repo.UpsertCartERPInvoice(ctx, p)
		if errors.Is(err, erp.ErrCartBusy) {
			continue // A newer observation or binding won the race.
		}
		if err != nil {
			s.logger.Warn("retained invoice application deferred", zap.String("cart_id", p.CartID), zap.Error(err))
			continue
		}
		if count > 0 {
			s.logger.Info("retained invoice applied to materialised order", zap.String("cart_id", p.CartID))
		}
	}
}

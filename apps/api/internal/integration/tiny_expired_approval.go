package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

const tinyApprovalAfterExpiry = providers.TinyApprovalAfterExpiry

// Expiration ends a local offer. An approval in Tiny is a subsequent merchant
// decision about the same sale. Recover it without issuing any ERP write.
func (s *Service) restoreExpiredTinyApproval(
	ctx context.Context, cartID, storeID, orderID string,
) (bool, error) {
	var eligible bool
	if err := s.repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM carts c
		JOIN live_events e ON e.id=c.event_id WHERE c.id=$1 AND e.store_id=$2
		AND c.external_order_id=$3 AND c.status='expired'
		AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded'))`,
		cartID, storeID, orderID,
	).Scan(&eligible); err != nil || !eligible {
		return false, err
	}
	integration, err := s.repo.GetActiveERP(ctx, storeID)
	if err != nil {
		return false, err
	}
	if integration.Provider != "tiny" {
		return false, nil
	}
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, erp.ErrCartBusy
	}
	defer release()
	provider, err := s.erpProviderFor(ctx, integration)
	if err != nil {
		return false, err
	}
	reader, ok := provider.(providers.ERPApprovalReader)
	if !ok {
		return false, fmt.Errorf("Tiny does not expose an approval snapshot")
	}
	snapshot, err := reader.GetOrderApprovalSnapshot(ctx, orderID)
	if err != nil {
		return false, fmt.Errorf("reading Tiny approval after expiry: %w", err)
	}
	if snapshot == nil || snapshot.OrderID != orderID || snapshot.TotalCents <= 0 || len(snapshot.Items) == 0 {
		return false, fmt.Errorf("incomplete Tiny approval snapshot")
	}
	// This method is reached only from a recorded approval. A later invoice is
	// compatible with that observation; a cancellation or reopened draft is not.
	switch snapshot.Status {
	case providers.ERPOrderStatusAprovado, providers.ERPOrderStatusFaturado,
		providers.ERPOrderStatusPreparandoEnvio, providers.ERPOrderStatusProntoEnvio,
		providers.ERPOrderStatusEnviado, providers.ERPOrderStatusEntregue:
	default:
		logger.From(ctx, s.logger).Info("stale Tiny approval did not restore an expired cart",
			zap.String("cart_id", cartID), zap.String("erp_status", string(snapshot.Status)))
		return false, nil
	}
	resolved := make(map[string]string)
	for _, line := range snapshot.Items {
		if line.ProductID == "" || line.Quantity <= 0 || line.UnitPrice < 0 {
			return false, fmt.Errorf("invalid Tiny approval item")
		}
		if _, found := resolved[line.ProductID]; found {
			continue
		}
		id, found, err := s.ResolveLocalProduct(ctx, storeID, line.ProductID)
		if err != nil {
			return false, err
		}
		if !found {
			id, err = s.ImportProductFromERP(ctx, storeID, line.ProductID)
			if err != nil {
				return false, err
			}
		}
		resolved[line.ProductID] = id
	}
	restored, err := s.repo.restoreExpiredTinyApproval(ctx, cartID, integration, snapshot, resolved)
	if restored && err == nil {
		logger.From(ctx, s.logger).Info("expired cart paid following verified Tiny approval",
			zap.String("cart_id", cartID), zap.String("external_order_id", orderID),
			zap.Int64("amount_cents", snapshot.TotalCents))
	}
	return restored, err
}

func (r *Repository) restoreExpiredTinyApproval(
	ctx context.Context, cartID string, integration *IntegrationRow,
	snapshot *providers.ERPApprovalSnapshot, resolved map[string]string,
) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var eligible bool
	var eventID, previousERPState string
	var expiresAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT c.status='expired' AND c.joined_to_cart_id IS NULL
		AND NOT c.purchase_closed AND NOT c.payment_review_required
		AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
		AND c.paid_amount_cents=0 AND c.external_order_id=$3
		AND e.store_id=$2::uuid AND EXISTS(SELECT 1 FROM integrations i
		 WHERE i.id=$4 AND i.store_id=e.store_id AND i.provider='tiny' AND i.status='active')
		AND NOT EXISTS(SELECT 1 FROM cart_payments cp WHERE cp.cart_id=c.id),
		c.event_id::text,c.erp_order_state,c.expires_at
		FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1 FOR UPDATE OF c`,
		cartID, integration.StoreID, snapshot.OrderID, integration.ID,
	).Scan(&eligible, &eventID, &previousERPState, &expiresAt)
	if err != nil || !eligible {
		return false, err
	}
	if err := cartedit.AssertReady(ctx, tx, cartID); err != nil {
		return false, err
	}
	var existingOrder bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE cart_id=$1)`, cartID).Scan(&existingOrder); err != nil {
		return false, err
	}
	if existingOrder {
		return false, fmt.Errorf("expired Tiny sale already has a materialized order; reconciliation required")
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM carts WHERE joined_to_cart_id=$1 ORDER BY id FOR UPDATE`, cartID); err != nil {
		return false, err
	}
	var conflictingChild bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM carts c WHERE joined_to_cart_id=$1
		AND (status='cancelled' OR payment_status IN ('paid','refunded')
		 OR payment_review_required OR paid_amount_cents>0 OR external_order_id IS NOT NULL
		 OR EXISTS(SELECT 1 FROM cart_payments WHERE cart_id=c.id)
		 OR EXISTS(SELECT 1 FROM orders WHERE cart_id=c.id)))`,
		cartID,
	).Scan(&conflictingChild); err != nil {
		return false, err
	}
	if conflictingChild {
		return false, fmt.Errorf("Tiny approval requires reconciliation of a linked purchase")
	}
	var unmapped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_items ci
		JOIN carts c ON c.id=ci.cart_id JOIN products p ON p.id=ci.product_id
		WHERE COALESCE(c.joined_to_cart_id,c.id)=$1 AND ci.quantity>0
		AND (COALESCE(p.external_id,'')='' OR p.external_source IS DISTINCT FROM 'tiny'))`, cartID).Scan(&unmapped); err != nil {
		return false, err
	}
	if unmapped {
		return false, fmt.Errorf("expired purchase contains products without a Tiny binding")
	}
	var previousItems json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT COALESCE(json_agg(json_build_object(
		'cart_id',ci.cart_id,'product_id',ci.product_id,
		'quantity',l.quantity-l.waitlisted_quantity,'unit_price_cents',l.unit_price)
		ORDER BY ci.cart_id,l.sequence),'[]'::json)
		FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id
		JOIN carts c ON c.id=ci.cart_id WHERE COALESCE(c.joined_to_cart_id,c.id)=$1`, cartID).Scan(&previousItems); err != nil {
		return false, err
	}
	// Flip status and payment together: another live may already have created a
	// new open cart for this buyer. Never reopen a payable offer in between.
	if _, err = tx.Exec(ctx, `UPDATE carts SET status='checkout',payment_status='paid',
		cancelled_reason=NULL,expires_at=NULL,erp_order_state='reflecting',
		cancellation_reverted_at=now(),cancellation_reverted_reason=$2,
		shipping_cost_cents=$3 WHERE id=$1`,
		cartID, tinyApprovalAfterExpiry, snapshot.FreightCents,
	); err != nil {
		return false, err
	}
	// Children remain closed source carts, with the payment owned by the host.
	if _, err = tx.Exec(ctx, `UPDATE carts SET status='checkout',cancelled_reason=NULL
		WHERE joined_to_cart_id=$1 AND status='expired' AND purchase_closed`, cartID); err != nil {
		return false, err
	}
	report, err := reflectCartPurchaseTx(
		ctx, tx, cartID, integration.StoreID, snapshot.OrderID, snapshot.Items, resolved,
	)
	if err != nil {
		return false, err
	}
	if report.Deferred > 0 {
		return false, fmt.Errorf("Tiny approval has %d unconfirmed product edits", report.Deferred)
	}
	now := time.Now().UTC()
	history := map[string]any{
		"previous_status": "expired", "previous_erp_state": previousERPState,
		"previous_expires_at": expiresAt.Time, "reconciled_at": now,
		"external_order_id": snapshot.OrderID, "erp_status": snapshot.Status,
		"previous_items": previousItems, "verified_total_cents": snapshot.TotalCents,
		"verified_freight_cents": snapshot.FreightCents, "invoice_id": snapshot.InvoiceID,
	}
	paid := providers.PaymentStatus{
		PaymentID: "erp-" + snapshot.OrderID, PaymentMethod: erpPaymentMethod,
		ExternalReference: cartID, Status: "paid", Amount: snapshot.TotalCents, PaidAt: &now,
		Metadata: map[string]any{tinyApprovalAfterExpiry: history},
	}
	var gmv int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM((l.quantity-l.waitlisted_quantity)*l.unit_price),0)
		FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id
		JOIN carts c ON c.id=ci.cart_id WHERE COALESCE(c.joined_to_cart_id,c.id)=$1`, cartID).Scan(&gmv); err != nil {
		return false, err
	}
	payload, err := json.Marshal(map[string]any{
		"cart_id": cartID, "store_id": integration.StoreID,
		"gmv_cents":  gmv,
		"payment_id": paid.PaymentID, "payment_method": paid.PaymentMethod, "payment_snapshot": paid,
	})
	if err != nil {
		return false, err
	}
	q := sqlc.New(tx)
	id, err := parseUUID(cartID)
	if err != nil {
		return false, err
	}
	if _, err = q.UpdateCartPayment(ctx, sqlc.UpdateCartPaymentParams{
		CartID: id, PaymentStatus: pgtype.Text{String: "paid", Valid: true},
		CheckoutID: paid.PaymentID, PaidAt: pgtype.Timestamptz{Time: now, Valid: true},
		PaymentMethod: pgtype.Text{String: erpPaymentMethod, Valid: true}, AmountCents: paid.Amount,
	}); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE cart_items SET paid_quantity=quantity-waitlisted_quantity
		WHERE cart_id IN (SELECT id FROM carts WHERE joined_to_cart_id=$1)`, cartID); err != nil {
		return false, err
	}
	// The ledger belongs to the owner, but covers the products from every source.
	if _, err = tx.Exec(ctx, `UPDATE cart_payments SET gross_covered_cents=$3
		WHERE cart_id=$1 AND checkout_id=$2`, cartID, paid.PaymentID, gmv+snapshot.FreightCents); err != nil {
		return false, err
	}
	store, err := parseUUID(integration.StoreID)
	if err != nil {
		return false, err
	}
	if _, err = q.RecordERPOrderStatus(ctx, sqlc.RecordERPOrderStatusParams{
		StoreID: store, ExternalOrderID: snapshot.OrderID, Status: string(snapshot.Status),
		Source: "reconciliation",
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE carts SET erp_order_state='confirmed',erp_op_started_at=NULL
		WHERE id=$1`, cartID); err != nil {
		return false, err
	}
	if err = events.Emit(ctx, q, events.Envelope{
		Name: events.CartPaid, Source: events.SourceInternal, LiveEventID: eventID,
		DedupKey: "cart.paid:erp:" + integration.StoreID + ":" + cartID + ":" + snapshot.OrderID,
		Payload:  payload,
	}); err != nil {
		return false, err
	}
	// The ERP may already have deducted these units before its approval arrives.
	// Subtracting them again would invent a shortage. Fence the local balance
	// until the durable stock reader obtains the current AVAILABLE quantity.
	externals := make([]string, 0, len(resolved))
	for external := range resolved {
		externals = append(externals, external)
	}
	sort.Strings(externals)
	for _, external := range externals {
		productID := resolved[external]
		if _, err = tx.Exec(ctx, `UPDATE products SET stock=LEAST(stock,0),erp_seq=erp_seq+1 WHERE id=$1`, productID); err != nil {
			return false, err
		}
		command := TinyProductWebhookCommand{
			StoreID: integration.StoreID, IntegrationID: integration.ID, ProductID: external, Kind: "estoque",
		}
		if err = tx.QueryRow(ctx, `INSERT INTO erp_stock_sync_state(product_id,requested_revision,last_attempt_at)
			VALUES($1,1,'epoch') ON CONFLICT(product_id) DO UPDATE
			SET requested_revision=erp_stock_sync_state.requested_revision+1,last_attempt_at='epoch'
			RETURNING requested_revision`, productID).Scan(&command.Revision); err != nil {
			return false, err
		}
		raw, err := json.Marshal(command)
		if err != nil {
			return false, err
		}
		if err = events.Emit(ctx, q, events.Envelope{
			Name: events.ERPWebhookProcess, Source: events.SourceTiny, Payload: raw,
			DedupKey: "tiny.expired-approval.stock:" + cartID + ":" + productID,
		}); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

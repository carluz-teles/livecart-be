package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
)

// The stock payload is an invalidation signal. Always read available stock;
// Tiny's webhook saldo is not the quantity that can be promised to a buyer.
type TinyProductWebhookCommand struct {
	StoreID       string `json:"store_id"`
	IntegrationID string `json:"integration_id"`
	ProductID     string `json:"product_id"`
	Kind          string `json:"kind"`
	Revision      int64  `json:"revision,omitempty"`
}

var errERPStockReadBusy = errors.New("stock read already in progress; retry with the durable invalidation")

func (s *Service) enqueueTinyProductWebhook(ctx context.Context, storeID, kind, productID string) error {
	integration, err := s.repo.GetActiveERP(ctx, storeID)
	if httpx.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving Tiny webhook owner: %w", err)
	}
	if integration.Provider != "tiny" {
		return nil
	}
	command := TinyProductWebhookCommand{StoreID: storeID, IntegrationID: integration.ID, ProductID: productID, Kind: kind}
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if kind == "estoque" {
		// Recovery claims integration -> stock checkpoint. Acquire the shared
		// integration first too, or ingress and recovery can deadlock each other.
		// Keep the ping in this transaction: it only reports a durable delivery.
		if _, err = tx.Exec(ctx, `UPDATE integrations SET metadata=COALESCE(metadata,'{}'::jsonb)||jsonb_build_object('stockWebhookLastPingAt',now()) WHERE id=$1`, integration.ID); err != nil {
			return err
		}
		// Invalidate a GET already in flight before persisting the new revision.
		// Lock order is product -> checkpoint, shared with waitlist allocation.
		if _, err = tx.Exec(ctx, `UPDATE products SET erp_seq=erp_seq+1 WHERE store_id=$1 AND external_source='tiny' AND external_id=$2`, storeID, productID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO erp_stock_sync_state(product_id,requested_revision,last_attempt_at)
            SELECT id,1,'epoch' FROM products WHERE store_id=$1 AND external_source='tiny' AND external_id=$2
            ON CONFLICT(product_id) DO UPDATE SET requested_revision=erp_stock_sync_state.requested_revision+1
            RETURNING requested_revision`, storeID, productID).Scan(&command.Revision)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return err
	}
	// Tiny does not send an event ID. Repeated balances (A -> B -> A) are real
	// transitions, so neither the product ID nor a payload hash can deduplicate them.
	if err = events.Emit(ctx, sqlc.New(tx), events.Envelope{Name: events.ERPWebhookProcess, Source: events.SourceTiny,
		DedupKey: "tiny.product:" + uuid.NewString(), Payload: payload,
		Metadata: map[string]string{"store_id": storeID, "integration_id": integration.ID},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) ProcessTinyProductWebhook(ctx context.Context, command TinyProductWebhookCommand) error {
	if command.StoreID == "" || command.IntegrationID == "" || command.ProductID == "" || (command.Kind != "estoque" && command.Kind != "produto") {
		return fmt.Errorf("invalid Tiny product command: %w", asynq.SkipRetry)
	}
	integration, err := s.repo.GetActiveERP(ctx, command.StoreID)
	if httpx.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if integration.Provider != "tiny" || integration.ID != command.IntegrationID {
		return nil
	}
	ctx = logger.WithStore(ctx, command.StoreID, "")
	var applied bool
	if command.Kind == "estoque" {
		applied, err = s.refreshERPAvailableStockRevision(ctx, integration, command.ProductID, command.Revision)
	} else {
		applied, err = s.ProcessProductWebhook(ctx, command.StoreID, "tiny", command.ProductID)
	}
	if errors.Is(err, errERPStockPendingEdit) {
		logger.From(ctx, s.logger).Info("Tiny stock webhook awaits order edit reconciliation",
			zap.String("external_product_id", command.ProductID))
		return nil // The durable product checkpoint now owns recovery.
	}
	if err != nil {
		return fmt.Errorf("processing Tiny product webhook: %w", err)
	}
	if applied {
		return s.ProcessWaitlistAfterStockWebhook(ctx, command.StoreID, "tiny", command.ProductID)
	}
	return nil
}

// Stock-only synchronization shares the account limiter, reservation deduction
// and optimistic version with manual/catalog sync. No product-detail GET or ERP write.
func (s *Service) refreshERPAvailableStock(ctx context.Context, integration *IntegrationRow, externalID string) (bool, error) {
	return s.refreshERPAvailableStockRevision(ctx, integration, externalID, 0)
}

func (s *Service) refreshERPAvailableStockRevision(ctx context.Context, integration *IntegrationRow, externalID string, revision int64) (bool, error) {
	// A read cannot outlive its shared lease. No database connection is held
	// while waiting for quota or for the ERP to respond.
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var id string
	var seen int64
	var previous int
	err := s.repo.pool.QueryRow(ctx, `SELECT id::text,erp_seq,stock FROM products WHERE store_id=$1 AND external_source=$2 AND external_id=$3`, integration.StoreID, integration.Provider, externalID).Scan(&id, &seen, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owner := uuid.NewString()
	var requested, completed int64
	var deferred bool
	err = s.repo.pool.QueryRow(ctx, `INSERT INTO erp_stock_sync_state(product_id,read_owner,read_until)
        VALUES($1,$2,now()+interval '2 minutes') ON CONFLICT(product_id) DO UPDATE
        SET read_owner=EXCLUDED.read_owner,read_until=EXCLUDED.read_until
        WHERE erp_stock_sync_state.read_until IS NULL OR erp_stock_sync_state.read_until<now()
        RETURNING requested_revision,completed_revision,deferred_at IS NOT NULL`, id, owner).Scan(&requested, &completed, &deferred)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errERPStockReadBusy
	}
	if err != nil {
		return false, fmt.Errorf("claiming stock read: %w", err)
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer releaseCancel()
		if _, err := s.repo.pool.Exec(releaseCtx, `UPDATE erp_stock_sync_state SET read_owner=NULL,read_until=NULL WHERE product_id=$1 AND read_owner=$2`, id, owner); err != nil {
			logger.From(releaseCtx, s.logger).Warn("stock read lease release deferred", zap.String("product_id", id), zap.Error(err))
		}
	}()
	if err := s.repo.deferStockForPendingEdit(ctx, id); err != nil {
		return false, err
	}
	if revision > 0 && completed >= revision && completed >= requested && !deferred {
		// Still run waitlist delivery if the previous attempt committed the
		// stock snapshot but failed during the subsequent promotion.
		return true, nil
	}
	// Read the CAS version after winning the lease, not before another reader
	// finishes. Other inventory writers retain the optimistic version guard.
	if err = s.repo.pool.QueryRow(ctx, `SELECT erp_seq,stock FROM products WHERE id=$1`, id).Scan(&seen, &previous); err != nil {
		return false, err
	}
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return false, err
	}
	reader, ok := provider.(providers.ERPStockReader)
	if !ok {
		return false, fmt.Errorf("ERP does not expose available stock")
	}
	available, err := reader.GetProductStock(ctx, externalID)
	if err != nil {
		return false, err
	}
	admissible := s.PortaoAPartirDoSaldoDoERP(ctx, integration, externalID, available)
	if admissible < 0 {
		return false, fmt.Errorf("pending reservations could not be read")
	}
	applied, err := s.repo.ApplyERPStockMirror(ctx, id, admissible, seen)
	if err != nil {
		return false, err
	}
	if !applied {
		_, checkpointErr := s.repo.pool.Exec(ctx, `UPDATE erp_stock_sync_state SET deferred_at=COALESCE(deferred_at,now()) WHERE product_id=$1`, id)
		if checkpointErr != nil {
			return false, fmt.Errorf("recording invalidated stock snapshot: %w", checkpointErr)
		}
		return false, fmt.Errorf("ERP stock snapshot invalidated by concurrent change")
	}
	if integration.Provider == "tiny" {
		_, err = s.repo.pool.Exec(ctx, `UPDATE erp_stock_sync_state SET last_success_at=now(),deferred_at=NULL,
            completed_revision=GREATEST(completed_revision,$3) WHERE product_id=$1 AND read_owner=$2`, id, owner, requested)
		if err != nil {
			return false, fmt.Errorf("recording successful stock check: %w", err)
		}
	}
	logger.From(ctx, s.logger).Info("ERP available stock reconciled", zap.String("store_id", integration.StoreID), zap.String("provider", integration.Provider), zap.String("product_id", id), zap.String("external_product_id", externalID), zap.Int("previous_stock", previous), zap.Int("erp_available", available), zap.Int("admissible", admissible), zap.Bool("changed", previous != admissible))
	return true, nil
}

// Claims at most ten products per account per minute. Checkpoints are bounded
// by catalog size. A failed/missing webhook no longer leaves stock stale forever.
func (s *Service) claimTinyStockChecks(ctx context.Context, storeID string) ([]string, error) {
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	// A shared one-minute lease prevents replica count from multiplying the scan budget.
	tag, err := tx.Exec(ctx, `UPDATE integrations SET metadata=COALESCE(metadata,'{}'::jsonb)||jsonb_build_object('stockRecoveryClaimedAt',now())
 WHERE store_id=$1 AND provider='tiny' AND status='active'
 AND COALESCE((metadata->>'stockRecoveryClaimedAt')::timestamptz,'epoch')<now()-interval '1 minute'`, storeID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return []string{}, nil
	}
	rows, err := tx.Query(ctx, `WITH eligible AS (
 SELECT p.id,s.last_attempt_at,
   CASE WHEN EXISTS(
     SELECT 1 FROM waitlist_items wi JOIN carts c ON c.id=wi.cart_id
     LEFT JOIN carts host ON host.id=c.joined_to_cart_id
     WHERE wi.product_id=p.id AND wi.status='waiting' AND wi.quantity>0
       AND c.status IN ('active','checkout') AND NOT c.purchase_closed
       AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
       AND (c.never_expires OR c.expires_at IS NULL OR c.expires_at>now())
       AND (host.id IS NULL OR (NOT host.purchase_closed AND host.status IN ('active','checkout')
         AND COALESCE(host.payment_status,'pending') NOT IN ('paid','refunded')))
   ) THEN 0 WHEN EXISTS(
     SELECT 1 FROM session_products sp JOIN live_sessions ls ON ls.id=sp.session_id
     JOIN live_events e ON e.id=ls.event_id
     WHERE sp.product_id=p.id AND ls.status IN ('active','live') AND e.status='active'
   ) OR EXISTS(
     SELECT 1 FROM cart_items ci JOIN carts c ON c.id=ci.cart_id JOIN live_events e ON e.id=c.event_id
     WHERE ci.product_id=p.id AND c.status IN ('active','checkout') AND NOT c.purchase_closed
       AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded') AND e.status='active'
   ) THEN 1 WHEN s.deferred_at IS NOT NULL OR s.requested_revision>s.completed_revision THEN 2 ELSE 3 END AS urgency
 FROM products p LEFT JOIN erp_stock_sync_state s ON s.product_id=p.id
 WHERE p.store_id=$1 AND p.external_source='tiny' AND p.active AND COALESCE(p.external_id,'')<>''
 AND NOT EXISTS(SELECT 1 FROM cart_erp_edit_requests r JOIN cart_erp_edits w ON w.cart_id=r.cart_id
     WHERE r.product_id=p.id AND r.revision>w.synced_revision)
 AND (s.last_attempt_at IS NULL OR s.last_attempt_at<now()-interval '5 minutes')
 AND (s.read_until IS NULL OR s.read_until<now())
 AND (s.deferred_at IS NOT NULL OR s.requested_revision>s.completed_revision OR s.last_success_at IS NULL OR s.last_success_at<now()-interval '15 minutes')
 ), priority AS (
 SELECT id,0 AS lane FROM eligible WHERE urgency<3 ORDER BY urgency,last_attempt_at NULLS FIRST,id LIMIT 8
 ), rotation AS (
 SELECT id,1 AS lane FROM eligible WHERE id NOT IN (SELECT id FROM priority)
 ORDER BY urgency DESC,last_attempt_at NULLS FIRST,id LIMIT (10-(SELECT COUNT(*) FROM priority))
 ), candidates AS (
 SELECT * FROM priority UNION ALL SELECT * FROM rotation
 ), claimed AS (
 INSERT INTO erp_stock_sync_state(product_id,last_attempt_at)
 SELECT id,now() FROM candidates
 ON CONFLICT(product_id) DO UPDATE SET last_attempt_at=now()
 WHERE erp_stock_sync_state.last_attempt_at<now()-interval '5 minutes'
 RETURNING product_id)
 SELECT p.external_id FROM claimed c JOIN products p ON p.id=c.product_id
 JOIN candidates selected ON selected.id=p.id JOIN eligible e ON e.id=p.id
 ORDER BY selected.lane,e.urgency,e.last_attempt_at NULLS FIRST,p.id`, storeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) RunTinyStockRecovery(ctx context.Context) {
	rows, err := s.repo.pool.Query(ctx, `SELECT store_id::text FROM integrations WHERE type='erp' AND provider='tiny' AND status='active' ORDER BY store_id`)
	if err != nil {
		s.logger.Warn("Tiny stock recovery cannot list accounts", zap.Error(err))
		return
	}
	stores := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		stores = append(stores, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.logger.Warn("Tiny stock recovery cannot read accounts", zap.Error(err))
		return
	}
	for _, storeID := range stores {
		if ctx.Err() != nil {
			return
		}
		func() {
			accountCtx, cancel := context.WithTimeout(logger.WithStore(ctx, storeID, ""), 40*time.Second)
			defer cancel()
			integration, err := s.repo.GetActiveERP(accountCtx, storeID)
			if err != nil || integration.Provider != "tiny" {
				return
			}
			ids, err := s.claimTinyStockChecks(accountCtx, storeID)
			if err != nil {
				s.logger.Warn("Tiny stock recovery cannot claim products", zap.Error(err))
				return
			}
			checked := 0
			for _, id := range ids {
				if accountCtx.Err() != nil {
					break
				}
				// The periodic scan can wait for an operator's lookup. Keep the
				// original context for subsequent waitlist/order processing.
				applied, readErr := s.refreshERPAvailableStock(ratelimit.WithTinyCatalogRead(accountCtx), integration, id)
				if readErr != nil {
					logger.From(accountCtx, s.logger).Warn("Tiny stock recovery deferred", zap.String("external_product_id", id), zap.Error(readErr))
					// A removed SKU must not starve the rest of the batch. The shared
					// account limiter and context still stop work during a cooldown.
					continue
				}
				if applied {
					checked++
					if err := s.ProcessWaitlistAfterStockWebhook(accountCtx, storeID, "tiny", id); err != nil {
						logger.From(accountCtx, s.logger).Warn("Tiny stock recovery waitlist deferred", zap.String("external_product_id", id), zap.Error(err))
					}
				}
			}
			if len(ids) > 0 {
				logger.From(accountCtx, s.logger).Info("Tiny stock recovery batch finished", zap.Int("claimed", len(ids)), zap.Int("checked", checked))
			}
		}()
	}
}

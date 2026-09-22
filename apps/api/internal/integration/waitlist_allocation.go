package integration

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/inventory"
)

// PromoteNextWaitlistEntry commits one allocation. Product serialization keeps
// FIFO across events; taking the cart lock before any product row lock matches
// checkout/payment transactions. A busy head is retried, never overtaken.
func (r *Repository) PromoteNextWaitlistEntry(ctx context.Context, storeID, productID string) (*inventory.WaitlistPromotion, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "waitlist_product:"+productID); err != nil {
		return nil, err
	}

	result := &inventory.WaitlistPromotion{StoreID: storeID, ProductID: productID}
	var ownerCartID string
	err = tx.QueryRow(ctx, `SELECT wi.id::text,wi.cart_id::text,wi.event_id::text,wi.platform_handle,
        wi.unit_price,wi.quantity,wi.fulfilled_quantity,COALESCE(wi.price_lot_id::text,''),COALESCE(c.joined_to_cart_id,c.id)::text
        FROM waitlist_items wi JOIN carts c ON c.id=wi.cart_id
        JOIN live_events cart_event ON cart_event.id=c.event_id
        LEFT JOIN carts host ON host.id=c.joined_to_cart_id
        JOIN live_events e ON e.id=wi.event_id JOIN products p ON p.id=wi.product_id
        WHERE wi.product_id=$1 AND e.store_id=$2 AND p.store_id=$2 AND cart_event.store_id=$2 AND p.active
          AND wi.status='waiting' AND wi.quantity>0
          AND NOT c.purchase_closed AND c.status IN ('active','checkout')
          AND c.payment_status IS DISTINCT FROM 'paid' AND c.payment_status IS DISTINCT FROM 'refunded'
          AND (c.never_expires OR c.expires_at IS NULL OR c.expires_at>now())
          AND (host.id IS NULL OR (host.status IN ('active','checkout')
            AND host.payment_status IS DISTINCT FROM 'paid' AND host.payment_status IS DISTINCT FROM 'refunded'
            AND (host.never_expires OR host.expires_at IS NULL OR host.expires_at>now())))
        ORDER BY wi.created_at,wi.queue_sequence LIMIT 1`, productID, storeID).Scan(
		&result.WaitlistItemID, &result.CartID, &result.EventID, &result.PlatformHandle,
		&result.UnitPrice, &result.Remaining, &result.Fulfilled, &result.PriceLotID, &ownerCartID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// A joined checkout belongs to its host. Payment locks that host first;
	// mirror this order and then protect the request's original child cart.
	cartsToLock := []string{ownerCartID}
	if result.CartID != ownerCartID {
		cartsToLock = append(cartsToLock, result.CartID)
	}
	for _, id := range cartsToLock {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte("erp_finalisation:" + id))
		var acquired bool
		if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(hash.Sum64())).Scan(&acquired); err != nil {
			return nil, err
		}
		if !acquired {
			return nil, inventory.ErrWaitlistPromotionDeferred
		}
		var eligible, review bool
		var currentOwner string
		if err = tx.QueryRow(ctx, `SELECT NOT purchase_closed AND status IN ('active','checkout')
            AND payment_status IS DISTINCT FROM 'paid' AND payment_status IS DISTINCT FROM 'refunded'
            AND (never_expires OR expires_at IS NULL OR expires_at>now()),payment_review_required,
            COALESCE(joined_to_cart_id,id)::text FROM carts WHERE id=$1 FOR UPDATE`, id).Scan(&eligible, &review, &currentOwner); err != nil {
			return nil, err
		}
		if !eligible || review || currentOwner != ownerCartID {
			return nil, inventory.ErrWaitlistPromotionDeferred
		}
	}

	var stock int
	if err = tx.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1 AND store_id=$2 AND active FOR UPDATE`, productID, storeID).Scan(&stock); err != nil {
		return nil, err
	}
	if stock <= 0 {
		return nil, nil
	}
	// A newer ERP invalidation makes the cached balance unsafe to promise.
	// Check under the product lock also taken by webhook ingress, so a stale
	// completed task cannot allocate stock while the newer read is pending.
	var staleStock bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM erp_stock_sync_state
        WHERE product_id=$1 AND (requested_revision>completed_revision OR deferred_at IS NOT NULL))`, productID).Scan(&staleStock); err != nil {
		return nil, err
	}
	if staleStock {
		return nil, inventory.ErrWaitlistPromotionDeferred
	}
	// Cancellation edits lock the cart too. Recheck the request after that lock
	// so a customer cannot receive units cancelled while we selected the head.
	var status string
	if err = tx.QueryRow(ctx, `SELECT status,quantity FROM waitlist_items WHERE id=$1 FOR UPDATE`, result.WaitlistItemID).Scan(&status, &result.Remaining); err != nil {
		return nil, err
	}
	if status != "waiting" || result.Remaining <= 0 {
		return nil, inventory.ErrWaitlistPromotionDeferred
	}
	var waiting int
	err = tx.QueryRow(ctx, `SELECT waitlisted_quantity FROM cart_items WHERE cart_id=$1 AND product_id=$2 FOR UPDATE`, result.CartID, productID).Scan(&waiting)
	if errors.Is(err, pgx.ErrNoRows) {
		// A legacy row may outlive a manually deleted cart item. The buyer
		// cancelled the line; closing only this orphan must not consume stock.
		if _, err = tx.Exec(ctx, `UPDATE waitlist_items SET cancelled_quantity=cancelled_quantity+quantity,
            quantity=0,status='cancelled',cancelled_at=now() WHERE id=$1`, result.WaitlistItemID); err != nil {
			return nil, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		result.Quantity = 0
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading queued cart line: %w", err)
	}
	if waiting < result.Remaining {
		return nil, fmt.Errorf("waitlist/cart quantity mismatch for request %s", result.WaitlistItemID)
	}
	result.Quantity = min(stock, result.Remaining)
	result.Remaining -= result.Quantity
	result.Fulfilled += result.Quantity
	if result.PriceLotID != "" {
		if _, err = tx.Exec(ctx, `SELECT set_config('livecart.waitlist_price_lot_id',$1,true)`, result.PriceLotID); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE products SET stock=stock-$2,erp_seq=erp_seq+1,updated_at=now() WHERE id=$1`, productID, result.Quantity); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE cart_items SET waitlisted_quantity=waitlisted_quantity-$3,
        erp_pending_since=CASE WHEN EXISTS(SELECT 1 FROM integrations WHERE store_id=$4 AND type='erp' AND status='active')
            THEN COALESCE(erp_pending_since,now()) ELSE erp_pending_since END,
        erp_confirmed_quantity=CASE WHEN erp_pending_since IS NULL AND NOT EXISTS(SELECT 1 FROM integrations WHERE store_id=$4 AND type='erp' AND status='active')
            THEN quantity-waitlisted_quantity+$3 ELSE COALESCE(erp_confirmed_quantity,quantity-waitlisted_quantity) END
        WHERE cart_id=$1 AND product_id=$2`, result.CartID, productID, result.Quantity, storeID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE waitlist_items SET quantity=CASE WHEN $2::int=0 THEN $3::int ELSE $2::int END,
        fulfilled_quantity=$3,status=CASE WHEN $2::int=0 THEN 'notified' ELSE 'waiting' END,
        notified_at=now(),expires_at=NULL WHERE id=$1`, result.WaitlistItemID, result.Remaining, result.Fulfilled); err != nil {
		return nil, err
	}
	qtx := sqlc.New(tx)
	dedup := fmt.Sprintf("waitlist.notified:%s:%d", result.WaitlistItemID, result.Fulfilled)
	if err = events.EmitInternal(ctx, qtx, events.WaitlistNotified, dedup, result); err != nil {
		return nil, err
	}
	if err = events.EmitInternal(ctx, qtx, events.StockReserved, "stock.reserved:"+dedup, struct {
		ProductID string `json:"product_id"`
		CartID    string `json:"cart_id"`
		EventID   string `json:"event_id"`
		Quantity  int    `json:"quantity"`
		Op        string `json:"op"`
	}{ProductID: productID, CartID: result.CartID, EventID: result.EventID, Quantity: result.Quantity, Op: "waitlist_promote"}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// CancelWaitingRequest removes only units still waiting. Already allocated
// products are normal cart items and must use the ordinary cart edit flow.
func (r *Repository) CancelWaitingRequest(ctx context.Context, id, cartID string) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT status IN ('active','checkout') AND payment_status IS DISTINCT FROM 'paid'
        AND payment_status IS DISTINCT FROM 'refunded' FROM carts WHERE id=$1 FOR UPDATE`, cartID).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}
	var productID, lotID string
	var quantity int
	err = tx.QueryRow(ctx, `SELECT product_id::text,quantity,COALESCE(price_lot_id::text,'')
        FROM waitlist_items WHERE id=$1 AND cart_id=$2 AND status='waiting' FOR UPDATE`, id, cartID).Scan(&productID, &quantity, &lotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cartWaiting int
	err = tx.QueryRow(ctx, `SELECT waitlisted_quantity FROM cart_items WHERE cart_id=$1 AND product_id=$2 FOR UPDATE`, cartID, productID).Scan(&cartWaiting)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil && cartWaiting < quantity {
		return false, fmt.Errorf("waitlist/cart quantity mismatch for request %s", id)
	}

	if lotID != "" {
		if _, err = tx.Exec(ctx, `SELECT set_config('livecart.waitlist_price_lot_id',$1,true)`, lotID); err != nil {
			return false, err
		}
	}
	// Close first: the price-lot mirror must not count this cancellation twice.
	if _, err = tx.Exec(ctx, `UPDATE waitlist_items SET status='cancelled',quantity=0,
        cancelled_quantity=cancelled_quantity+$2,cancelled_at=now() WHERE id=$1`, id, quantity); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_id=$1 AND product_id=$2 AND quantity=$3 AND waitlisted_quantity=$3`, cartID, productID, quantity); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE cart_items SET quantity=quantity-$3,waitlisted_quantity=waitlisted_quantity-$3
        WHERE cart_id=$1 AND product_id=$2 AND quantity>$3 AND waitlisted_quantity>=$3`, cartID, productID, quantity); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ProcessWaitlistProduct is the durable queue consumer entry point.
func (s *Service) ProcessWaitlistProduct(ctx context.Context, storeID, productID string) error {
	return s.inventory().ProcessWaitlistProduct(ctx, storeID, productID)
}

func (r *Repository) enqueueReopenedWaitlistLots(ctx context.Context, tx pgx.Tx, cart sqlc.Cart, productID, cartID pgtype.UUID) error {
	rows, err := tx.Query(ctx, `SELECT l.id::text,l.waitlisted_quantity,l.unit_price
        FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id
        WHERE ci.cart_id=$1 AND ci.product_id=$2 AND l.waitlisted_quantity>0 ORDER BY l.sequence`, cartID, productID)
	if err != nil {
		return err
	}
	type waitingLot struct {
		id       string
		quantity int
		price    int64
	}
	lots := []waitingLot{}
	for rows.Next() {
		var lot waitingLot
		if err = rows.Scan(&lot.id, &lot.quantity, &lot.price); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, lot)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, lot := range lots {
		var requestID string
		err = tx.QueryRow(ctx, `INSERT INTO waitlist_items(event_id,product_id,platform_user_id,platform_handle,
            quantity,position,cart_id,unit_price,price_lot_id)
            VALUES($1,$2,$3,$4,$5,(SELECT COALESCE(MAX(position),0)+1 FROM waitlist_items WHERE event_id=$1 AND product_id=$2),$6,$7,$8)
            RETURNING id::text`, cart.EventID, productID, cart.PlatformUserID, cart.PlatformHandle, lot.quantity, cartID, lot.price, lot.id).Scan(&requestID)
		if err != nil {
			return err
		}
		if err = events.EmitInternal(ctx, sqlc.New(tx), events.WaitlistQueued, "waitlist.queued:"+requestID,
			struct {
				EventID   string `json:"event_id"`
				ProductID string `json:"product_id"`
				CartID    string `json:"cart_id"`
			}{
				EventID: uuidToString(cart.EventID), ProductID: uuidToString(productID), CartID: uuidToString(cartID),
			}); err != nil {
			return err
		}
	}
	return nil
}

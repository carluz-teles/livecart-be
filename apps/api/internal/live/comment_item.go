package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
)

var _ commentItemWriter = (*Service)(nil)

func (s *Service) ApplyCommentItem(ctx context.Context, input AddToCartInput, commentID string) (CommentItemResult, error) {
	if previous, err := s.repo.appliedCommentItem(ctx, s.repo.pool, commentID, input.ProductID); err == nil {
		return previous, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return CommentItemResult{}, err
	}
	cart, isNew, err := s.getOrCreateCartForItem(ctx, input)
	if err != nil {
		return CommentItemResult{}, err
	}
	return s.repo.applyCommentItem(ctx, input, commentID, cart, isNew)
}

type commentQuerier = sqlc.DBTX

func (r *Repository) appliedCommentItem(ctx context.Context, q commentQuerier, commentID, productID string) (CommentItemResult, error) {
	var result CommentItemResult
	var status string
	err := q.QueryRow(ctx, `SELECT e.cart_id::text,c.token,e.quantity,e.waitlisted_quantity,e.is_new_cart,c.status
        FROM cart_item_events e JOIN carts c ON c.id=e.cart_id
        WHERE e.platform_comment_id=$1 AND e.product_id=$2`, commentID, productID).Scan(
		&result.CartID, &result.CartToken, &result.Quantity, &result.WaitlistedQuantity, &result.IsNewCart, &status)
	if err != nil {
		return result, err
	}
	result.AlreadyApplied = true
	if status == "cancelled" || status == "expired" {
		result.Quantity = 0
		result.SkipReason = "cart_terminated"
		return result, nil
	}
	id, parseErr := parseUUID(result.CartID)
	if parseErr != nil {
		return result, parseErr
	}
	totals, totalsErr := sqlc.New(q).GetCartTotals(ctx, id)
	result.TotalItems, result.TotalCents, err = int(totals.TotalItems), totals.TotalValue, totalsErr
	return result, err
}

// Stock, the cart line, its attribution, the waitlist and the idempotency key
// commit together. A failed transaction cannot consume stock without an item.
func (r *Repository) applyCommentItem(ctx context.Context, input AddToCartInput, commentID string, cart *CartRow, isNew bool) (CommentItemResult, error) {
	result := CommentItemResult{AddToCartOutput: AddToCartOutput{CartID: cart.ID, CartToken: cart.Token, IsNewCart: isNew}}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "comment:"+commentID); err != nil {
		return result, err
	}
	if previous, e := r.appliedCommentItem(ctx, tx, commentID, input.ProductID); e == nil {
		return previous, nil
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return result, e
	}
	var payable bool
	if err := tx.QueryRow(ctx, `SELECT status NOT IN ('cancelled','expired') AND COALESCE(payment_status,'pending') NOT IN ('paid','refunded') FROM carts WHERE id=$1 FOR UPDATE`, cart.ID).Scan(&payable); err != nil {
		return result, err
	}
	if !payable {
		return result, fmt.Errorf("cart changed before accepting item")
	}
	var stock, maximum, current int
	if err = tx.QueryRow(ctx, `SELECT p.stock,COALESCE(s.cart_max_quantity_per_item,0)
        FROM products p JOIN stores s ON s.id=p.store_id
        WHERE p.id=$1 AND p.store_id=$2 AND p.active FOR UPDATE OF p`, input.ProductID, input.StoreID).Scan(&stock, &maximum); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT quantity FROM cart_items WHERE cart_id=$1 AND product_id=$2),0)`, cart.ID, input.ProductID).Scan(&current); err != nil {
		return result, err
	}
	result.MaxQuantity = maximum
	quantity := input.Quantity
	if maximum > 0 {
		quantity = min(quantity, max(0, maximum-current))
	}
	if quantity <= 0 {
		result.SkipReason = "max_quantity_reached"
		return result, nil
	}
	available := min(quantity, max(0, stock))
	waiting := quantity - available
	var alreadyWaiting bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM waitlist_items WHERE event_id=$1 AND product_id=$2 AND platform_user_id=$3 AND status='waiting')`, input.EventID, input.ProductID, input.PlatformUserID).Scan(&alreadyWaiting); err != nil {
		return result, err
	}
	if alreadyWaiting {
		waiting = 0
	}
	if available+waiting == 0 {
		result.SkipReason = "already_waitlisted"
		return result, nil
	}
	if available > 0 {
		if _, err = tx.Exec(ctx, `UPDATE products SET stock=stock-$2,erp_seq=erp_seq+1,updated_at=now() WHERE id=$1`, input.ProductID, available); err != nil {
			return result, err
		}
	}
	cartID, err := parseUUID(cart.ID)
	if err != nil {
		return result, err
	}
	productID, err := parseUUID(input.ProductID)
	if err != nil {
		return result, err
	}
	var sessionID pgtype.UUID
	if input.SessionID != "" {
		sessionID, err = parseUUID(input.SessionID)
		if err != nil {
			return result, err
		}
	}
	qtx := r.q.WithTx(tx)
	if _, err = qtx.UpsertCartItem(ctx, sqlc.UpsertCartItemParams{CartID: cartID, ProductID: productID,
		Quantity: pgtype.Int4{Int32: int32(available + waiting), Valid: true}, UnitPrice: pgtype.Int8{Int64: input.ProductPrice, Valid: true},
		WaitlistedQuantity: int32(waiting), SessionID: sessionID}); err != nil {
		return result, err
	}
	if available > 0 {
		if _, err = tx.Exec(ctx, `UPDATE cart_items SET erp_pending_since=COALESCE(erp_pending_since,now()),
            erp_confirmed_quantity=COALESCE(erp_confirmed_quantity,GREATEST(0,quantity-waitlisted_quantity-$3))
            WHERE cart_id=$1 AND product_id=$2`, cart.ID, input.ProductID, available); err != nil {
			return result, err
		}
	}
	if waiting > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO waitlist_items(event_id,product_id,platform_user_id,platform_handle,quantity,position,cart_id)
            VALUES ($1,$2,$3,$4,$5,(SELECT COALESCE(MAX(position),0)+1 FROM waitlist_items WHERE event_id=$1 AND product_id=$2),$6)`,
			input.EventID, input.ProductID, input.PlatformUserID, input.PlatformHandle, waiting, cart.ID)
		if err != nil {
			return result, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cart_item_events(cart_id,product_id,session_id,quantity,unit_price,platform_comment_id,waitlisted_quantity,is_new_cart)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, cartID, productID, sessionID, available+waiting, input.ProductPrice, commentID, waiting, isNew); err != nil {
		return result, err
	}
	payload, err := json.Marshal(struct {
		CartID    string `json:"cart_id"`
		ProductID string `json:"product_id"`
		Quantity  int    `json:"quantity"`
		SessionID string `json:"session_id,omitempty"`
	}{cart.ID, input.ProductID, available + waiting, input.SessionID})
	if err != nil {
		return result, err
	}
	if err = events.Emit(ctx, qtx, events.Envelope{Name: events.CartItemAdded, Source: events.SourceInternal,
		DedupKey: fmt.Sprintf("comment.item:%s:%s", commentID, input.ProductID), Payload: payload}); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Quantity = available + waiting
	result.WaitlistedQuantity = waiting
	result.TotalItems, result.TotalCents, err = r.GetCartTotals(ctx, cart.ID)
	return result, err
}

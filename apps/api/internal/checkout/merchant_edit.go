package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
)

type merchantEditPayload struct {
	Operation string `json:"operation"`
	ItemID    string `json:"itemId,omitempty"`
	ProductID string `json:"productId,omitempty"`
	Quantity  int    `json:"quantity"`
}

// Queue only the Tiny merchant flow. Older clients without an idempotency key
// retain their synchronous contract; other providers use their existing flow.
func (s *Service) queueMerchantEdit(ctx context.Context, input MutateCartItemInput, operation string) (bool, error) {
	key := cartedit.RequestID(ctx)
	if !input.ByMerchant || key == "" || s.pool == nil {
		return false, nil
	}
	if _, err := uuid.Parse(key); err != nil {
		return true, httpx.DomainError(400, httpx.CodeValidationFailed, "Idempotency-Key inválida")
	}
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return true, err
	}
	req := merchantEditPayload{Operation: operation, ItemID: input.ItemID, ProductID: input.ProductID, Quantity: input.Quantity}
	raw, err := json.Marshal(req)
	if err != nil {
		return true, err
	}
	started := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM carts WHERE id=$1 FOR UPDATE`, cart.ID); err != nil {
		return true, err
	}
	r := NewRepository(s.repo.q.WithTx(tx))
	cart, err = r.GetCartByToken(ctx, input.Token)
	if err != nil {
		return true, err
	}
	// A lost HTTP response must not turn an accepted edit into another addition.
	var previousCart string
	var same bool
	err = tx.QueryRow(ctx, `SELECT cart_id::text,request=$2::jsonb FROM cart_erp_edit_requests WHERE id=$1`, key, string(raw)).Scan(&previousCart, &same)
	if err == nil {
		if previousCart != cart.ID || !same {
			return true, httpx.DomainError(409, httpx.CodeIdempotencyKeyReused, "esta chave já foi usada para outra alteração")
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return true, err
	}
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM integrations i WHERE i.store_id=$1
        AND i.provider='tiny' AND i.status='active') AND EXISTS(SELECT 1 FROM carts c WHERE c.id=$2
        AND c.external_order_id IS NOT NULL AND c.joined_to_cart_id IS NULL)`, cart.StoreID, cart.ID).Scan(&eligible); err != nil {
		return true, err
	}
	if !eligible {
		return false, nil
	}
	if err := assertCartMutable(cart, toggleGovernsMerchantItemEdit, time.Now()); err != nil {
		return true, err
	}
	var state string
	var processing, joined, review bool
	if err := tx.QueryRow(ctx, `SELECT erp_order_state,EXISTS(SELECT 1 FROM cart_erp_edits w
        WHERE w.cart_id=c.id AND w.lease_until>now()), EXISTS(SELECT 1 FROM carts child WHERE child.joined_to_cart_id=c.id) OR c.joined_to_cart_id IS NOT NULL
         ,c.payment_review_required FROM carts c WHERE id=$1`, cart.ID).Scan(&state, &processing, &joined, &review); err != nil {
		return true, err
	}
	if joined {
		return false, nil
	}
	if review || cart.PaymentStatus == "refunded" {
		return true, httpx.DomainError(409, httpx.CodePaymentReviewRequired, "o pagamento deste pedido requer conferência antes de editar")
	}
	if processing || state != "open" {
		return true, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "o pedido está sincronizando; aguarde antes de editar novamente")
	}
	var item *CartItemRow
	if operation != "add" {
		item, err = r.GetCartItem(ctx, input.ItemID)
		if err != nil {
			return true, err
		}
		if item.CartID != cart.ID {
			return true, httpx.DomainError(404, httpx.CodeCartItemNotFound, "item não encontrado neste carrinho")
		}
		input.ProductID = item.ProductID
	} else {
		item, err = r.FindCartItemByProduct(ctx, cart.ID, input.ProductID)
		if err != nil {
			return true, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM products WHERE id=$1 FOR UPDATE`, input.ProductID); err != nil {
		return true, err
	}
	// Re-read after the stock lock: a comment may have changed the line while
	// this request was waiting for another cart's reservation to commit.
	if _, err := tx.Exec(ctx, `SELECT id FROM cart_items WHERE cart_id=$1 AND product_id=$2 FOR UPDATE`, cart.ID, input.ProductID); err != nil {
		return true, err
	}
	if operation == "add" {
		item, err = r.FindCartItemByProduct(ctx, cart.ID, input.ProductID)
	} else {
		item, err = r.GetCartItem(ctx, input.ItemID)
	}
	if err != nil {
		return true, err
	}
	cfg, err := r.GetEventProductForCart(ctx, cart.EventID, cart.StoreID, input.ProductID)
	if err != nil {
		return true, err
	}
	var linked, hasWaitlist bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(p.external_source='tiny' AND p.external_id IS NOT NULL,false),
      EXISTS(SELECT 1 FROM waitlist_items wi WHERE wi.cart_id=$2 AND wi.product_id=p.id AND wi.status IN ('waiting','notified'))
      FROM products p WHERE p.id=$1`, input.ProductID, cart.ID).Scan(&linked, &hasWaitlist); err != nil {
		return true, err
	}
	// Waitlist promotion has its own reservation lifecycle. Preserve that
	// existing path instead of treating a promotion as a merchant reservation.
	if !linked || hasWaitlist || (item != nil && item.WaitlistedQuantity > 0) {
		return false, nil
	}
	before, waiting, price := 0, 0, cfg.UnitPrice
	if item != nil {
		before, waiting, price = item.Quantity, item.WaitlistedQuantity, item.UnitPrice
	}
	after := input.Quantity
	switch operation {
	case "add":
		if !cfg.Active {
			return true, httpx.DomainError(422, httpx.CodeValidationFailed, "produto não está ativo")
		}
		after = before + input.Quantity
	case "remove":
		after = 0
	}
	if after < 0 || (operation != "remove" && input.Quantity < 1) {
		return true, httpx.DomainError(422, httpx.CodeValidationFailed, "quantidade inválida")
	}
	if after > before && cfg.MaxQuantity > 0 && after > cfg.MaxQuantity {
		return true, httpx.DomainError(422, httpx.CodeValidationFailed, fmt.Sprintf("limite de %d por item", cfg.MaxQuantity))
	}
	_, waitAfter, delta := splitQuantityChange(before, waiting, after)
	// Preserve the initial quote before changing any line.
	if err := r.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cart_erp_edits(cart_id) VALUES($1) ON CONFLICT DO NOTHING`, cart.ID); err != nil {
		return true, err
	}
	var retained int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(r.retained_quantity),0) FROM cart_erp_edit_requests r
        JOIN cart_erp_edits w ON w.cart_id=r.cart_id WHERE r.cart_id=$1 AND r.product_id=$2 AND r.revision>w.synced_revision`, cart.ID, input.ProductID).Scan(&retained); err != nil {
		return true, err
	}
	retainDelta := -delta
	reserved := 0
	if delta > 0 {
		reused := min(retained, delta)
		retainDelta = -reused
		reserved = delta - reused
		n, err := tx.Exec(ctx, `UPDATE products SET stock=stock-$2,erp_seq=erp_seq+1,updated_at=now()
            WHERE id=$1 AND stock >= $2`, input.ProductID, delta-reused)
		if err != nil {
			return true, err
		}
		if n.RowsAffected() == 0 {
			return true, httpx.DomainError(422, httpx.CodeStockInsufficient, "estoque insuficiente para esse aumento")
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE products SET erp_seq=erp_seq+1 WHERE id=$1`, input.ProductID); err != nil {
			return true, err
		}
	}
	if after == 0 {
		if err := r.DeleteCartItem(ctx, item.ID); err != nil {
			return true, err
		}
	} else if item != nil {
		ok, err := r.SetCartItemSplitIfUnchanged(ctx, item.ID, before, after, waitAfter)
		if err != nil {
			return true, err
		}
		if !ok {
			return true, httpx.DomainError(409, httpx.CodeCartItemChanged, "o item mudou; atualize o pedido")
		}
	} else {
		if _, err := r.CreateCartItem(ctx, cart.ID, input.ProductID, after, price); err != nil {
			return true, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE cart_items SET erp_confirmed_quantity=COALESCE(erp_confirmed_quantity,$3),
        erp_pending_since=COALESCE(erp_pending_since,now()) WHERE cart_id=$1 AND product_id=$2`, cart.ID, input.ProductID, before-waiting); err != nil {
		return true, err
	}
	if waitAfter < waiting {
		if _, err := tx.Exec(ctx, `UPDATE waitlist_items SET status='cancelled' WHERE cart_id=$1 AND product_id=$2 AND status IN ('waiting','notified')`, cart.ID, input.ProductID); err != nil {
			return true, err
		}
	}
	var revision int64
	if err := tx.QueryRow(ctx, `UPDATE cart_erp_edits SET revision=revision+1,
        queued_at=CASE WHEN revision=synced_revision THEN now() ELSE queued_at END,
        next_attempt_at=now()+interval '1 second',last_error=NULL WHERE cart_id=$1 RETURNING revision`, cart.ID).Scan(&revision); err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cart_erp_edit_requests(id,cart_id,revision,request,product_id,retained_quantity)
        VALUES($1,$2,$3,$4::jsonb,$5,$6)`, key, cart.ID, revision, string(raw), input.ProductID, retainDelta); err != nil {
		return true, err
	}
	kind := "quantity_decreased"
	if after > before {
		kind = "quantity_increased"
	}
	if before == 0 {
		kind = "item_added"
	}
	if after == 0 {
		kind = "item_removed"
	}
	if err := recordMutation(ctx, r.q, MutationParams{CartID: cart.ID, ProductID: input.ProductID, MutationType: kind,
		QuantityBefore: before, QuantityAfter: after, UnitPrice: price, Source: "merchant"}); err != nil {
		return true, err
	}
	if reserved > 0 {
		if err := emitMerchantStockEvent(ctx, r.q, events.StockReserved, key, cart.ID, cart.EventID, input.ProductID, reserved); err != nil {
			return true, err
		}
	}
	if err := r.UpdateCartShipping(ctx, tx, cart.ID, nil); err != nil {
		return true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	s.logger.Info("merchant edit queued", zap.String("cart_id", cart.ID), zap.String("store_id", cart.StoreID),
		zap.Int64("revision", revision), zap.String("operation", operation), zap.Duration("enqueue_duration", time.Since(started)))
	return true, nil
}

// Stock facts share the transaction that changes the local balance. Request
// and revision keys distinguish separate edits without duplicating retries.
func emitMerchantStockEvent(ctx context.Context, q *sqlc.Queries, name events.Name, key, cartID, eventID, productID string, quantity int) error {
	op := "qty_increase"
	if name == events.StockReleased {
		op = "qty_decrease"
	}
	return events.EmitInternal(ctx, q, name, string(name)+":merchant_edit:"+key, struct {
		Op        string `json:"op"`
		ProductID string `json:"product_id"`
		Quantity  int    `json:"quantity"`
		CartID    string `json:"cart_id"`
		EventID   string `json:"event_id"`
	}{op, productID, quantity, cartID, eventID})
}

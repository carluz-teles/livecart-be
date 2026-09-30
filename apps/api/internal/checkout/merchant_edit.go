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
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
)

// Execution metadata is persisted at acceptance; losing an integration later
// does not turn an outstanding remote reservation into a local-only edit.
type cartEditExecution = cartedit.Execution

type merchantEditPayload struct {
	Operation    string `json:"operation"`
	ItemID       string `json:"itemId,omitempty"`
	ProductID    string `json:"productId,omitempty"`
	Quantity     int    `json:"quantity"`
	OriginCartID string `json:"originCartId,omitempty"`
	Source       string `json:"source,omitempty"`
}

// Every stock edit uses the same durable allocation. Missing HTTP idempotency
// keys must never bypass the transaction protecting the stock mirror.
func (s *Service) queueMerchantEdit(ctx context.Context, input MutateCartItemInput, operation string) (bool, error) {
	key := cartedit.RequestID(ctx)
	if key == "" {
		key = uuid.NewString()
	}
	if _, err := uuid.Parse(key); err != nil {
		return true, httpx.DomainError(400, httpx.CodeValidationFailed, "Idempotency-Key inválida")
	}
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return true, err
	}
	var ownerID string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(joined_to_cart_id,id)::text FROM carts WHERE id=$1`, cart.ID).Scan(&ownerID); err != nil {
		return true, err
	}
	req := merchantEditPayload{Operation: operation, ItemID: input.ItemID, ProductID: input.ProductID, Quantity: input.Quantity}
	req.OriginCartID = cart.ID
	if !input.ByMerchant {
		req.Source = mutationSource(false)
	}
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "merchant_edit_request:"+key); err != nil {
		return true, err
	}
	// A lost HTTP response must not turn an accepted edit into another addition.
	var same bool
	err = tx.QueryRow(ctx, `SELECT (request - '_execution' - 'originCartId')=($2::jsonb - 'originCartId')
 AND COALESCE(NULLIF(request->>'originCartId',''),cart_id::text)=$3
 FROM cart_erp_edit_requests WHERE id=$1`, key, string(raw), cart.ID).Scan(&same)
	if err == nil {
		if !same {
			return true, httpx.DomainError(409, httpx.CodeIdempotencyKeyReused, "esta chave já foi usada para outra alteração")
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return true, err
	}
	if err := cartedit.LockTopologyForEdit(ctx, tx, cart.ID, ownerID); err != nil {
		return true, err
	}
	// Product admission uses the same serialization as global FIFO. Resolve
	// ownership before locking; re-read the item below after the cart lock.
	if input.ProductID == "" {
		if err := tx.QueryRow(ctx, `SELECT product_id::text FROM cart_items WHERE id=$1 AND cart_id=$2`, input.ItemID, cart.ID).Scan(&input.ProductID); err != nil {
			return true, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "waitlist_product:"+input.ProductID); err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM carts WHERE id IN ($1,$2) ORDER BY (id=$1) DESC,id FOR UPDATE`, ownerID, cart.ID); err != nil {
		return true, err
	}
	var currentOwner string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(joined_to_cart_id,id)::text FROM carts WHERE id=$1`, cart.ID).Scan(&currentOwner); err != nil {
		return true, err
	}
	if currentOwner != ownerID {
		return true, httpx.DomainError(409, httpx.CodeCartItemChanged, "a compra mudou; atualize o pedido")
	}
	r := NewRepository(s.repo.q.WithTx(tx))
	cart, err = r.GetCartByToken(ctx, input.Token)
	if err != nil {
		return true, err
	}
	if err := assertCartMutable(cart, itemEditPolicy(input.ByMerchant), time.Now()); err != nil {
		return true, err
	}
	if err := assertCartItemsEditable(cart); err != nil {
		return true, err
	}
	var ownerToken string
	if err := tx.QueryRow(ctx, `SELECT token FROM carts WHERE id=$1`, ownerID).Scan(&ownerToken); err != nil {
		return true, err
	}
	owner, err := r.GetCartByToken(ctx, ownerToken)
	if err != nil {
		return true, err
	}
	if err := assertCartMutable(owner, false, time.Now()); err != nil {
		return true, err
	}
	if err := assertCartItemsEditable(owner); err != nil {
		return true, err
	}
	var state string
	var processing, review, blocked bool
	if err := tx.QueryRow(ctx, `SELECT erp_order_state,EXISTS(SELECT 1 FROM cart_erp_edits w
 WHERE w.cart_id=c.id AND w.lease_until>now()),
 EXISTS(SELECT 1 FROM carts member WHERE member.id IN ($1,$2) AND
 (member.payment_review_required OR member.payment_status='refunded')),
 EXISTS(SELECT 1 FROM cart_erp_edits w WHERE w.cart_id=c.id AND w.blocked_at IS NOT NULL
 AND w.revision>w.synced_revision) FROM carts c WHERE id=$1`, ownerID, cart.ID).Scan(&state, &processing, &review, &blocked); err != nil {
		return true, err
	}
	if review {
		return true, httpx.DomainError(409, httpx.CodePaymentReviewRequired, "o pagamento deste pedido requer conferência antes de editar")
	}
	if blocked {
		return true, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "as alterações deste pedido precisam de conciliação com o ERP antes de editar novamente")
	}
	if processing || (state != "open" && state != "none") {
		return true, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "o pedido está sincronizando; aguarde antes de editar novamente")
	}
	execution := cartEditExecution{Version: 1}
	var metadata []byte
	err = tx.QueryRow(ctx, `SELECT id::text,provider,COALESCE(metadata,'{}'::jsonb) FROM integrations
 WHERE store_id=$1 AND type='erp' AND status='active'`, cart.StoreID).Scan(&execution.IntegrationID, &execution.Provider, &metadata)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return true, err
	}
	if err == nil {
		var settings map[string]any
		if err := json.Unmarshal(metadata, &settings); err != nil {
			return true, err
		}
		execution.Remote = state != "none" || erp.ModoDeReservaDaIntegracao(execution.Provider, settings) == erp.ReservaNativaDoERP
	} else if state != "none" {
		return true, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "reative a integração deste pedido antes de editar")
	}
	// A pending batch cannot change provider or drop a remote obligation.
	var incompatible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_erp_edit_requests r JOIN cart_erp_edits w ON w.cart_id=r.cart_id
 WHERE r.cart_id=$1 AND r.revision>w.synced_revision AND (
 (r.request->'_execution' IS NULL AND NOT $2) OR
 (r.request->'_execution' IS NOT NULL AND (COALESCE((r.request->'_execution'->>'remote')::boolean,false)<>$2
 OR COALESCE(r.request->'_execution'->>'integrationId','')<>$3))))`, ownerID, execution.Remote, execution.IntegrationID).Scan(&incompatible); err != nil {
		return true, err
	}
	if incompatible {
		return true, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "conclua as alterações pendentes antes de trocar a integração")
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
	if operation != "add" && item != nil && item.WaitlistedQuantity > 0 {
		return true, httpx.DomainError(409, httpx.CodeCartItemChanged, "encerre a espera deste produto antes de alterar seus itens")
	}
	before, waiting, price := 0, 0, cfg.UnitPrice
	if item != nil {
		before, waiting, price = item.Quantity, item.WaitlistedQuantity, item.UnitPrice
	}
	after := input.Quantity
	switch operation {
	case "add":
		if !productAllowedForCart(cfg, input.ByMerchant) {
			return true, httpx.DomainError(422, httpx.CodeValidationFailed, "produto não disponível neste evento")
		}
		price = cfg.UnitPrice
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
	if _, err := tx.Exec(ctx, `INSERT INTO cart_erp_edits(cart_id) VALUES($1) ON CONFLICT DO NOTHING`, ownerID); err != nil {
		return true, err
	}
	var retained int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(r.retained_quantity),0) FROM cart_erp_edit_requests r
        JOIN cart_erp_edits w ON w.cart_id=r.cart_id WHERE r.cart_id=$1 AND r.product_id=$2 AND r.revision>w.synced_revision
 AND COALESCE(r.request->>'originCartId',r.cart_id::text)=$3`, ownerID, input.ProductID, cart.ID).Scan(&retained); err != nil {
		return true, err
	}
	retainDelta := -delta
	reserved := 0
	if delta > 0 {
		var olderWaiting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM waitlist_items wi JOIN carts c ON c.id=wi.cart_id LEFT JOIN carts host ON host.id=c.joined_to_cart_id
           WHERE wi.product_id=$1 AND wi.status='waiting' AND c.status IN ('active','checkout')
             AND c.payment_status IS DISTINCT FROM 'paid' AND c.payment_status IS DISTINCT FROM 'refunded'
             AND (c.never_expires OR c.expires_at IS NULL OR c.expires_at>now())
 AND (host.id IS NULL OR (host.status IN ('active','checkout') AND host.payment_status IS DISTINCT FROM 'paid' AND host.payment_status IS DISTINCT FROM 'refunded' AND (host.never_expires OR host.expires_at IS NULL OR host.expires_at>now()))))`, input.ProductID).Scan(&olderWaiting); err != nil {
			return true, err
		}
		if olderWaiting {
			return true, httpx.DomainError(409, httpx.CodeStockInsufficient, "a reposição está sendo destinada aos clientes na fila")
		}

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
		var ok bool
		var err error
		if operation == "add" {
			ok, err = r.AddCartItemQuantityAtPrice(ctx, item.ID, before, input.Quantity, price)
		} else {
			ok, err = r.SetCartItemSplitIfUnchanged(ctx, item.ID, before, after, waitAfter)
		}
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
	var revision int64
	if err := tx.QueryRow(ctx, `UPDATE cart_erp_edits SET revision=revision+1,
        queued_at=CASE WHEN revision=synced_revision THEN now() ELSE queued_at END,
        next_attempt_at=now()+interval '1 second',last_error=NULL WHERE cart_id=$1 RETURNING revision`, ownerID).Scan(&revision); err != nil {
		return true, err
	}
	var journal map[string]any
	if err := json.Unmarshal(raw, &journal); err != nil {
		return true, err
	}
	journal["_execution"] = execution
	raw, err = json.Marshal(journal)
	if err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cart_erp_edit_requests(id,cart_id,revision,request,product_id,retained_quantity)
        VALUES($1,$2,$3,$4::jsonb,$5,$6)`, key, ownerID, revision, string(raw), input.ProductID, retainDelta); err != nil {
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
		QuantityBefore: before, QuantityAfter: after, UnitPrice: price, Source: mutationSource(input.ByMerchant)}); err != nil {
		return true, err
	}
	if reserved > 0 {
		if err := emitMerchantStockEvent(ctx, r.q, events.StockReserved, key, cart.ID, cart.EventID, input.ProductID, reserved); err != nil {
			return true, err
		}
	}
	if input.ByMerchant {
		if err := r.UpdateCartShipping(ctx, tx, cart.ID, nil); err != nil {
			return true, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	s.logger.Info("cart edit queued", zap.String("cart_id", cart.ID), zap.String("store_id", cart.StoreID),
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

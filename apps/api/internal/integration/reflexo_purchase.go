package integration

import (
	"context"
	"fmt"
	"sort"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
)

// Resolve the complete remote grid before changing any cart. An unavailable
// product import must not leave half of a grouped purchase reflected.
func (s *Service) ReflectCartPurchase(ctx context.Context, cartID, storeID, orderID string, grid []providers.ERPOrderItem) (*erp.CartSyncReport, error) {
	resolved := make(map[string]string)
	imported := 0
	for _, line := range grid {
		if line.ProductID == "" || line.Quantity < 0 || line.UnitPrice < 0 {
			return nil, fmt.Errorf("invalid ERP order item for reflection")
		}
		if _, ok := resolved[line.ProductID]; ok {
			continue
		}
		id, found, err := s.ResolveLocalProduct(ctx, storeID, line.ProductID)
		if err != nil {
			return nil, err
		}
		if !found {
			id, err = s.ImportProductFromERP(ctx, storeID, line.ProductID)
			if err != nil {
				return nil, fmt.Errorf("importing reflection product %s: %w", line.ProductID, err)
			}
			imported++
		}
		resolved[line.ProductID] = id
	}
	report, err := s.repo.reflectCartPurchase(ctx, cartID, storeID, orderID, grid, resolved)
	if report != nil {
		report.Imported = imported
	}
	return report, err
}

type reflectionMember struct {
	cartID  string
	prices  map[int64]int
	pending bool
}

func (r *Repository) reflectCartPurchase(ctx context.Context, cartID, storeID, orderID string, grid []providers.ERPOrderItem, resolved map[string]string) (*erp.CartSyncReport, error) {
	report := &erp.CartSyncReport{CartID: cartID}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT c.joined_to_cart_id IS NULL AND c.external_order_id=$2
        AND c.erp_order_state='reflecting' AND c.status NOT IN ('cancelled','expired')
        AND COALESCE(c.store_id,e.store_id)=$3::uuid
        FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1 FOR UPDATE OF c`, cartID, orderID, storeID).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, fmt.Errorf("purchase binding changed during ERP reflection: %w", erp.ErrCartBusy)
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM carts WHERE joined_to_cart_id=$1 ORDER BY id FOR UPDATE`, cartID); err != nil {
		return nil, err
	}
	var edits bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_erp_edits w JOIN carts c ON c.id=w.cart_id
        WHERE COALESCE(c.joined_to_cart_id,c.id)=$1 AND w.revision>w.synced_revision)`, cartID).Scan(&edits); err != nil {
		return nil, err
	}
	if edits {
		return nil, fmt.Errorf("merchant edit pending during reflection: %w", erp.ErrCartBusy)
	}
	productIDs := make([]string, 0, len(resolved))
	for _, id := range resolved {
		productIDs = append(productIDs, id)
	}
	if _, err = tx.Exec(ctx, `SELECT p.id FROM products p WHERE p.id=ANY($2::uuid[]) OR EXISTS(
        SELECT 1 FROM cart_items ci JOIN carts c ON c.id=ci.cart_id
        WHERE ci.product_id=p.id AND COALESCE(c.joined_to_cart_id,c.id)=$1) ORDER BY p.id FOR UPDATE`, cartID, productIDs); err != nil {
		return nil, err
	}
	if err = confirmERPGridTx(ctx, tx, cartID, grid); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `SELECT p.external_id,ci.product_id::text,ci.cart_id::text,
        ci.erp_pending_since IS NOT NULL,l.unit_price,SUM(l.quantity-l.waitlisted_quantity)::int
        FROM cart_items ci JOIN carts c ON c.id=ci.cart_id JOIN products p ON p.id=ci.product_id
        JOIN cart_item_price_lots l ON l.cart_item_id=ci.id
        WHERE COALESCE(c.joined_to_cart_id,c.id)=$1 AND COALESCE(p.external_id,'')<>''
        GROUP BY p.external_id,ci.product_id,ci.cart_id,ci.erp_pending_since,l.unit_price
        ORDER BY MIN(l.created_at),MIN(l.sequence),ci.cart_id,l.unit_price`, cartID)
	if err != nil {
		return nil, err
	}
	local := make(map[string][]*reflectionMember)
	for rows.Next() {
		var external, product, cart string
		var pending bool
		var price int64
		var quantity int
		if err = rows.Scan(&external, &product, &cart, &pending, &price, &quantity); err != nil {
			rows.Close()
			return nil, err
		}
		resolved[external] = product
		var member *reflectionMember
		for _, m := range local[external] {
			if m.cartID == cart {
				member = m
				break
			}
		}
		if member == nil {
			member = &reflectionMember{cartID: cart, prices: make(map[int64]int), pending: pending}
			local[external] = append(local[external], member)
		}
		if quantity > 0 {
			member.prices[price] += quantity
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	remote := make(map[string]map[int64]int)
	for _, line := range grid {
		if remote[line.ProductID] == nil {
			remote[line.ProductID] = make(map[int64]int)
		}
		if line.Quantity > 0 {
			remote[line.ProductID][line.UnitPrice] += line.Quantity
		}
	}
	keys := make([]string, 0, len(resolved))
	for external := range resolved {
		keys = append(keys, external)
	}
	sort.Strings(keys)
	for _, external := range keys {
		members := local[external]
		before := make(map[int64]int)
		pending := false
		for _, m := range members {
			pending = pending || m.pending
			for price, qty := range m.prices {
				before[price] += qty
			}
		}
		if pending {
			report.Deferred++
			continue
		}
		if equalPurchasePrices(before, remote[external]) {
			continue
		}
		targets := distributePurchasePrices(cartID, members, remote[external])
		carts := make([]string, 0, len(targets))
		for cart := range targets {
			carts = append(carts, cart)
		}
		sort.Strings(carts)
		for _, cart := range carts {
			var lines []providers.ERPOrderItem
			for price, qty := range targets[cart] {
				if qty > 0 {
					lines = append(lines, providers.ERPOrderItem{ProductID: external, Quantity: qty, UnitPrice: price})
				}
			}
			if err = reflectCartItemPriceLines(ctx, tx, cart, resolved[external], lines); err != nil {
				return nil, err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE products SET erp_seq=erp_seq+1 WHERE id=$1`, resolved[external]); err != nil {
			return nil, err
		}
		from, to := 0, 0
		for _, qty := range before {
			from += qty
		}
		for _, qty := range remote[external] {
			to += qty
		}
		kind := "quantity"
		if from == 0 {
			kind = "added"
		}
		if to == 0 {
			kind = "removed"
		}
		report.Changes = append(report.Changes, erp.CartSyncChange{ExternalProductID: external, ProductID: resolved[external], Kind: kind, From: from, To: to})
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return report, nil
}

func equalPurchasePrices(a, b map[int64]int) bool {
	if len(a) != len(b) {
		return false
	}
	for price, qty := range a {
		if b[price] != qty {
			return false
		}
	}
	return true
}

// Keep matching units on their original carts first. Repricing also preserves
// cart attribution; only genuinely additional ERP units belong to the owner.
func distributePurchasePrices(owner string, members []*reflectionMember, desired map[int64]int) map[string]map[int64]int {
	remaining := make(map[int64]int, len(desired))
	prices := make([]int64, 0, len(desired))
	for price, qty := range desired {
		remaining[price] = qty
		prices = append(prices, price)
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
	targets := make(map[string]map[int64]int)
	for _, m := range members {
		targets[m.cartID] = make(map[int64]int)
		for price, qty := range m.prices {
			keep := min(qty, remaining[price])
			targets[m.cartID][price] = keep
			remaining[price] -= keep
		}
	}
	for _, m := range members {
		capacity := 0
		for price, qty := range m.prices {
			capacity += qty - targets[m.cartID][price]
		}
		for _, price := range prices {
			keep := min(capacity, remaining[price])
			targets[m.cartID][price] += keep
			remaining[price] -= keep
			capacity -= keep
		}
	}
	if targets[owner] == nil {
		targets[owner] = make(map[int64]int)
	}
	for price, qty := range remaining {
		targets[owner][price] += qty
	}
	return targets
}

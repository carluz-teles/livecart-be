package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/internal/integration/providers"
)

// SetCartItemPriceLines reflects allocated ERP units without repricing or
// consuming units still waiting for stock. Existing matching lots keep origin.
func (s *Service) SetCartItemPriceLines(ctx context.Context, cartID, productID string, lines []providers.ERPOrderItem) error {
	return s.repo.SetCartItemPriceLines(ctx, cartID, productID, lines)
}

func (r *Repository) SetCartItemPriceLines(ctx context.Context, cartID, productID string, lines []providers.ERPOrderItem) error {
	desired := make(map[int64]int)
	total := 0
	for _, line := range lines {
		if line.Quantity < 0 || line.UnitPrice < 0 {
			return fmt.Errorf("ERP price line has negative quantity or price")
		}
		desired[line.UnitPrice] += line.Quantity
		total += line.Quantity
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM carts WHERE id=$1 FOR UPDATE`, cartID).Scan(&status); err != nil {
		return err
	}
	if status == "expired" || status == "cancelled" {
		return nil
	}
	var itemID string
	var pending int
	var unconfirmed bool
	err = tx.QueryRow(ctx, `SELECT id::text,waitlisted_quantity,erp_pending_since IS NOT NULL FROM cart_items WHERE cart_id=$1 AND product_id=$2 FOR UPDATE`, cartID, productID).Scan(&itemID, &pending, &unconfirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		if total == 0 {
			return nil
		}
		if err = tx.QueryRow(ctx, `INSERT INTO cart_items(cart_id,product_id,quantity,waitlisted_quantity,unit_price)
		    VALUES($1,$2,0,0,0) RETURNING id::text`, cartID, productID).Scan(&itemID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// An unmatched pending write is our purchase awaiting ERP confirmation,
	// not a merchant edit. The preceding exact-grid acknowledgement clears it
	// only when every price/quantity is verified.
	if unconfirmed {
		return nil
	}
	type lot struct {
		id                string
		quantity, waiting int
		price             int64
	}
	rows, err := tx.Query(ctx, `SELECT id::text,quantity,waitlisted_quantity,unit_price FROM cart_item_price_lots
	    WHERE cart_item_id=$1 ORDER BY created_at,sequence FOR UPDATE`, itemID)
	if err != nil {
		return err
	}
	var lots []lot
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.id, &l.quantity, &l.waiting, &l.price); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, l := range lots {
		keep := min(l.quantity-l.waiting, desired[l.price])
		desired[l.price] -= keep
		if keep+l.waiting != l.quantity {
			if _, err = tx.Exec(ctx, `UPDATE cart_item_price_lots SET quantity=$2 WHERE id=$1`, l.id, keep+l.waiting); err != nil {
				return err
			}
		}
	}
	prices := make([]int64, 0, len(desired))
	for price := range desired {
		prices = append(prices, price)
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
	for _, price := range prices {
		if desired[price] == 0 {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO cart_item_price_lots(cart_item_id,quantity,waitlisted_quantity,unit_price)
		    VALUES($1,$2,0,$3)`, itemID, desired[price], price); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('livecart.price_lots_override',$1,true)`, itemID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE cart_items SET quantity=$2,erp_confirmed_quantity=$3,erp_pending_since=NULL,
	    unit_price=COALESCE((SELECT unit_price FROM cart_item_price_lots WHERE cart_item_id=$1 AND quantity>0 ORDER BY created_at,sequence LIMIT 1),unit_price)
	    WHERE id=$1`, itemID, total+pending, total); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('livecart.price_lots_override','',true)`); err != nil {
		return err
	}
	if total+pending == 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM cart_items WHERE id=$1`, itemID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

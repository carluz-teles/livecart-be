package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

type cartItemMutationState struct {
	item      *CartItemRow
	financial []byte
}

// Capture the result before releasing the mutation's locks. Reading it after
// commit could mistake a concurrent promotion for part of our own edit.
func (s *Service) mutateCartItemWithSnapshot(
	ctx context.Context,
	before *CartItemRow,
	mutate func(*Repository) error,
) (*cartItemMutationState, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	financial, err := lockCartForCompensation(ctx, tx, before.CartID)
	if err != nil {
		return nil, err
	}
	if err := matchCartItemSnapshot(ctx, tx, before); err != nil {
		return nil, err
	}
	repo := NewRepository(sqlc.New(tx))
	if err := mutate(repo); err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_items WHERE id=$1)`, before.ID).Scan(&exists); err != nil {
		return nil, err
	}
	state := &cartItemMutationState{financial: financial}
	if exists {
		state.item, err = repo.GetCartItem(ctx, before.ID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *Service) createCartItemWithSnapshot(
	ctx context.Context,
	cartID, productID string,
	quantity int,
	unitPrice int64,
) (*cartItemMutationState, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	financial, err := lockCartForCompensation(ctx, tx, cartID)
	if err != nil {
		return nil, err
	}
	repo := NewRepository(sqlc.New(tx))
	id, err := repo.CreateCartItem(ctx, cartID, productID, quantity, unitPrice)
	if err != nil {
		return nil, err
	}
	item, err := repo.GetCartItem(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &cartItemMutationState{item: item, financial: financial}, nil
}

func lockCartForCompensation(ctx context.Context, tx pgx.Tx, cartID string) ([]byte, error) {
	var mutable bool
	var financial []byte
	err := tx.QueryRow(ctx, `SELECT NOT purchase_closed AND status NOT IN ('expired','cancelled')
        AND payment_status IS DISTINCT FROM 'paid' AND payment_status IS DISTINCT FROM 'refunded'
        AND NOT payment_review_required,
        jsonb_build_object('status',status,'payment_status',payment_status,'paid_at',paid_at,
          'paid_amount_cents',paid_amount_cents,'payment_review_required',payment_review_required,
          'purchase_closed',purchase_closed)
        FROM carts WHERE id=$1 FOR UPDATE`, cartID).Scan(&mutable, &financial)
	if err != nil {
		return nil, err
	}
	if !mutable {
		return nil, httpx.DomainError(409, httpx.CodeCartItemChanged, "o carrinho mudou; atualize antes de editar")
	}
	return financial, nil
}

func matchCartItemSnapshot(ctx context.Context, tx pgx.Tx, item *CartItemRow) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM cart_items WHERE id=$1 FOR UPDATE`, item.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.DomainError(409, httpx.CodeCartItemChanged, "o item mudou; atualize o carrinho")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM cart_item_price_lots
        WHERE cart_item_id=$1 ORDER BY sequence FOR UPDATE`, item.ID); err != nil {
		return err
	}
	var matches bool
	err = tx.QueryRow(ctx, `SELECT to_jsonb(ci)=$2::jsonb AND
        (SELECT COALESCE(jsonb_agg(to_jsonb(pl) ORDER BY pl.sequence),'[]'::jsonb)
         FROM cart_item_price_lots pl WHERE pl.cart_item_id=ci.id)=$3::jsonb
        FROM cart_items ci WHERE ci.id=$1`, item.ID, string(item.OriginalItem), string(item.OriginalLots)).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return httpx.DomainError(409, httpx.CodeCartItemChanged, "o item mudou; atualize o carrinho")
	}
	return nil
}

// Restore the exact agreed prices after a synchronous ERP edit fails. A newer
// edit wins: never overwrite it with an older compensation snapshot.
func (s *Service) restoreCartItem(ctx context.Context, item *CartItemRow, expected *cartItemMutationState) error {
	if expected == nil {
		return fmt.Errorf("missing cart mutation snapshot")
	}
	if item == nil && expected.item == nil {
		return fmt.Errorf("missing cart item snapshot")
	}
	cartID := ""
	if item != nil {
		cartID = item.CartID
	} else {
		cartID = expected.item.CartID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	financial, err := lockCartForCompensation(ctx, tx, cartID)
	if err != nil {
		return err
	}
	if !bytes.Equal(financial, expected.financial) {
		return fmt.Errorf("cart changed before restoring item")
	}
	if expected.item == nil {
		tag, restoreErr := tx.Exec(ctx, `INSERT INTO cart_items SELECT (jsonb_populate_record(NULL::cart_items,$1::jsonb)).*
            ON CONFLICT DO NOTHING`, string(item.OriginalItem))
		if restoreErr != nil {
			return restoreErr
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("item was replaced before restoring its prices")
		}
	} else {
		if err := matchCartItemSnapshot(ctx, tx, expected.item); err != nil {
			return err
		}
		if item == nil {
			if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE id=$1`, expected.item.ID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		tag, restoreErr := tx.Exec(ctx, `UPDATE cart_items SET quantity=$2,waitlisted_quantity=$3
            WHERE id=$1`, item.ID, item.Quantity, item.WaitlistedQuantity)
		if restoreErr != nil {
			return restoreErr
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("item changed before restoring its prices")
		}
	}
	// Keep surviving IDs (and their waitlist references). New rollback-generated
	// lots have zero units; the original lots are restored with their timestamps.
	if _, err = tx.Exec(ctx, `UPDATE cart_item_price_lots SET quantity=0,waitlisted_quantity=0
        WHERE cart_item_id=$1 AND id NOT IN (SELECT (x->>'id')::uuid FROM jsonb_array_elements($2::jsonb) x)`, item.ID, string(item.OriginalLots)); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cart_item_price_lots SELECT * FROM jsonb_populate_recordset(NULL::cart_item_price_lots,$1::jsonb)
        ON CONFLICT(id) DO UPDATE SET quantity=EXCLUDED.quantity,waitlisted_quantity=EXCLUDED.waitlisted_quantity,
          unit_price=EXCLUDED.unit_price,session_id=EXCLUDED.session_id,created_at=EXCLUDED.created_at`, string(item.OriginalLots)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

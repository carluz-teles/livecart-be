//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestReflectionState_PreservesPaidRestingState(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_order_state='confirmed' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	won, err := testRepo.TransitionCartERPOrderState(ctx, fx.cartID, "confirmed", "reflecting")
	if err != nil || !won {
		t.Fatalf("claim reflection: won=%v err=%v", won, err)
	}
	if won, err := testRepo.TransitionCartERPOrderState(ctx, fx.cartID, "confirmed", "mutating"); err != nil || won {
		t.Fatalf("concurrent writer: won=%v err=%v", won, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_op_started_at=now()-interval '10 minutes' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	ops, err := testRepo.ListStuckERPOrderOps(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, op := range ops {
		if op.CartID != fx.cartID {
			continue
		}
		found = true
		if op.State != "reflecting" || op.RestingState != "confirmed" {
			t.Fatalf("recovery lost resting state: %+v", op)
		}
		if won, err := testRepo.TransitionCartERPOrderState(ctx, fx.cartID, op.State, op.RestingState); err != nil || !won {
			t.Fatalf("restore: won=%v err=%v", won, err)
		}
	}
	if !found {
		t.Fatal("interrupted reflection is absent from recovery")
	}
	var state string
	var cleared bool
	if err := testPool.QueryRow(ctx, `SELECT erp_order_state,erp_op_resting_state IS NULL FROM carts WHERE id=$1`, fx.cartID).Scan(&state, &cleared); err != nil {
		t.Fatal(err)
	}
	if state != "confirmed" || !cleared {
		t.Fatalf("state=%s cleared=%v", state, cleared)
	}
}

func TestConfirmERPGrid_DoesNotAcknowledgeNewerQuantity(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	ctx := context.Background()
	var external string
	if err := testPool.QueryRow(ctx, `SELECT external_id FROM products WHERE id=$1`, fx.productID).Scan(&external); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=0 WHERE cart_id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.ConfirmERPGrid(ctx, fx.cartID, []providers.ERPOrderItem{{ProductID: external, Quantity: 1, UnitPrice: 1000}}); err != nil {
		t.Fatal(err)
	}
	var pending bool
	var confirmed int
	if err := testPool.QueryRow(ctx, `SELECT erp_pending_since IS NOT NULL,erp_confirmed_quantity FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&pending, &confirmed); err != nil {
		t.Fatal(err)
	}
	if !pending || confirmed != 1 {
		t.Fatalf("older grid cleared newer delta: pending=%v confirmed=%d", pending, confirmed)
	}
	if err := testRepo.ConfirmERPGrid(ctx, fx.cartID, []providers.ERPOrderItem{{ProductID: external, Quantity: 2, UnitPrice: 1000}}); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT erp_pending_since IS NOT NULL FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("exact grid remains pending")
	}
}

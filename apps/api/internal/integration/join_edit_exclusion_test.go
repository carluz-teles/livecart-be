package integration

import (
	"context"
	"errors"
	"testing"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration/providers"
)

type joinUndoERP struct {
	*scriptedERP
	inspect func(context.Context)
}

func (p *joinUndoERP) SetOrderSituacao(context.Context, string, int) error {
	return errors.New("old order refuses cancellation")
}

func (p *joinUndoERP) UpdateOrderItems(ctx context.Context, id string, items []providers.ERPOrderItem) error {
	p.inspect(ctx)
	return p.scriptedERP.UpdateOrderItems(ctx, id, items)
}

func TestCartJoinKeepsEditExclusionThroughUndo(t *testing.T) {
	requireDB(t)
	host, child, _, _, store := semearParaJuntar(t)
	if _, err := testPool.Exec(t.Context(), `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'erp','tiny','active')`, store); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET created_at=now()-interval '1 day' WHERE id=$1`, host); err != nil {
		t.Fatal(err)
	}
	observedUndo := false
	fake := &joinUndoERP{scriptedERP: newScriptedERP()}
	fake.inspect = func(ctx context.Context) {
		var linked bool
		if err := testPool.QueryRow(ctx, `SELECT joined_to_cart_id IS NOT NULL FROM carts WHERE id=$1`, child).Scan(&linked); err != nil {
			t.Fatal(err)
		}
		if linked {
			return
		}
		observedUndo = true
		for _, id := range []string{host, child} {
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = cartedit.LockTopologyForEdit(ctx, tx, id)
			_ = tx.Rollback(ctx)
			if err == nil {
				t.Errorf("accepted edit on %s during undo ERP call", id)
			}
		}
	}
	svc := newFinalisationService(fake)
	if _, err := svc.JoinCarts(t.Context(), JoinCartsInput{StoreID: store, CartAID: host, CartBID: child}); err == nil {
		t.Fatal("expected rejected merge")
	}
	if !observedUndo {
		t.Fatal("did not reach ERP correction after detaching origin")
	}
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := cartedit.LockTopologyForEdit(t.Context(), tx, host, child); err != nil {
		t.Fatalf("join leaked lock: %v", err)
	}
}

func TestCartJoinRejectsAlreadyAcceptedEdit(t *testing.T) {
	requireDB(t)
	host, child, _, _, store := semearParaJuntar(t)
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_erp_edits(cart_id,revision) VALUES($1,1)`, child); err != nil {
		t.Fatal(err)
	}
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	if _, err := svc.JoinCarts(t.Context(), JoinCartsInput{StoreID: store, CartAID: host, CartBID: child}); err == nil {
		t.Fatal("joined a purchase with pending edit")
	}
	var linked bool
	if err := testPool.QueryRow(t.Context(), `SELECT joined_to_cart_id IS NOT NULL FROM carts WHERE id=$1`, child).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked || len(fake.callsWithPrefix("")) != 0 {
		t.Fatal("pending edit allowed join effects")
	}
}

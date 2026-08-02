package inventory_test

// Fatia B3a — testes puros (sem DB) do inventory.Service. Provam que os dois
// fluxos migrados (ListActiveWaitlistByCart, CancelWaitlistItem) delegam ao port
// InventoryRepository e aos WaitlistCollaborators corretamente, cobrindo sucesso
// e erro. Espelham o estilo de fake-port de internal/erp/service_test.go.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/inventory"
)

// fakeRepo implementa inventory.InventoryRepository, gravando as chamadas e
// devolvendo valores/erros configuráveis.
type fakeRepo struct {
	listRows   []inventory.ListActiveByCartRow
	listErr    error
	item       *inventory.WaitlistItemRow
	getItemErr error
	cancelErr  error
	cart       *inventory.CartRef

	cancelCalls int
	decCalls    int
}

func (r *fakeRepo) ListActiveByCart(context.Context, string) ([]inventory.ListActiveByCartRow, error) {
	return r.listRows, r.listErr
}
func (r *fakeRepo) GetWaitlistItemForCart(context.Context, string, string) (*inventory.WaitlistItemRow, error) {
	return r.item, r.getItemErr
}
func (r *fakeRepo) CancelWaitlistItem(context.Context, string, string) error {
	r.cancelCalls++
	return r.cancelErr
}
func (r *fakeRepo) DecrementCartItem(context.Context, string, string, int) (int, error) {
	r.decCalls++
	return 0, nil
}
func (r *fakeRepo) GetCartByID(context.Context, string) (*inventory.CartRef, error) {
	return r.cart, nil
}

// fakeCollab implementa inventory.WaitlistCollaborators.
type fakeCollab struct {
	adjustCalls  int
	adjustDelta  int
	processCalls int
	processArgs  [3]string
}

func (c *fakeCollab) AdjustStockReservationDelta(_ context.Context, _, _, _, _ string, delta int, _ int64, _ string, _ erp.StockOp) (string, error) {
	c.adjustCalls++
	c.adjustDelta = delta
	return "mov-1", nil
}
func (c *fakeCollab) ProcessWaitlistForProduct(_ context.Context, eventID, productID, storeID string) {
	c.processCalls++
	c.processArgs = [3]string{eventID, productID, storeID}
}

// noopStockRepo satisfies the (unexported) erp stock repo so we can build a real
// *erp.StockReservations — the cancel flows under test that hit cart!=nil never
// call Release, so its methods stay no-ops here.
type noopStockRepo struct{}

func (noopStockRepo) IncrementProductStock(context.Context, string, int) error  { return nil }
func (noopStockRepo) EmitStockReserved(context.Context, erp.StockEventParams) error {
	return nil
}
func (noopStockRepo) EmitStockReleased(context.Context, erp.StockEventParams) error {
	return nil
}

func newTestService(repo *fakeRepo, collab *fakeCollab) *inventory.Service {
	stock := erp.NewStockReservations(noopStockRepo{}, zap.NewNop())
	return inventory.NewService(repo, collab, stock, zap.NewNop())
}

func TestService_ListActiveWaitlistByCart(t *testing.T) {
	t.Run("success returns the repo rows", func(t *testing.T) {
		repo := &fakeRepo{listRows: []inventory.ListActiveByCartRow{{ID: "w1"}, {ID: "w2"}}}
		svc := newTestService(repo, &fakeCollab{})

		got, err := svc.ListActiveWaitlistByCart(context.Background(), "cart-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].ID != "w1" || got[1].ID != "w2" {
			t.Fatalf("rows = %+v, want [w1 w2]", got)
		}
	})

	t.Run("error is propagated", func(t *testing.T) {
		want := errors.New("db down")
		repo := &fakeRepo{listErr: want}
		svc := newTestService(repo, &fakeCollab{})

		if _, err := svc.ListActiveWaitlistByCart(context.Background(), "cart-1"); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})
}

func TestService_CancelWaitlistItem(t *testing.T) {
	t.Run("notified with cart reverses reservation and promotes queue", func(t *testing.T) {
		repo := &fakeRepo{
			item: &inventory.WaitlistItemRow{Status: "notified", ProductID: "p1", Quantity: 3, EventID: "e1"},
			cart: &inventory.CartRef{StoreID: "s1", PlatformHandle: "buyer"},
		}
		collab := &fakeCollab{}
		svc := newTestService(repo, collab)

		changed, err := svc.CancelWaitlistItem(context.Background(), "wl-1", "cart-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("changed = false, want true")
		}
		if repo.cancelCalls != 1 || repo.decCalls != 1 {
			t.Fatalf("cancelCalls=%d decCalls=%d, want 1 and 1", repo.cancelCalls, repo.decCalls)
		}
		if collab.adjustCalls != 1 || collab.adjustDelta != -3 {
			t.Fatalf("adjustCalls=%d delta=%d, want 1 and -3", collab.adjustCalls, collab.adjustDelta)
		}
		if collab.processCalls != 1 || collab.processArgs != [3]string{"e1", "p1", "s1"} {
			t.Fatalf("processCalls=%d args=%v, want 1 and [e1 p1 s1]", collab.processCalls, collab.processArgs)
		}
	})

	t.Run("waiting only cancels, no stock movement", func(t *testing.T) {
		repo := &fakeRepo{item: &inventory.WaitlistItemRow{Status: "waiting", ProductID: "p1", Quantity: 1}}
		collab := &fakeCollab{}
		svc := newTestService(repo, collab)

		changed, err := svc.CancelWaitlistItem(context.Background(), "wl-1", "cart-1")
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want true nil", changed, err)
		}
		if repo.cancelCalls != 1 || repo.decCalls != 0 || collab.adjustCalls != 0 || collab.processCalls != 0 {
			t.Fatalf("unexpected side effects: cancel=%d dec=%d adjust=%d process=%d",
				repo.cancelCalls, repo.decCalls, collab.adjustCalls, collab.processCalls)
		}
	})

	t.Run("missing item is a no-op", func(t *testing.T) {
		repo := &fakeRepo{item: nil}
		svc := newTestService(repo, &fakeCollab{})

		changed, err := svc.CancelWaitlistItem(context.Background(), "wl-1", "cart-1")
		if err != nil || changed {
			t.Fatalf("changed=%v err=%v, want false nil", changed, err)
		}
		if repo.cancelCalls != 0 {
			t.Fatalf("cancelCalls=%d, want 0 (nothing to cancel)", repo.cancelCalls)
		}
	})

	t.Run("load error is wrapped and propagated", func(t *testing.T) {
		want := errors.New("db down")
		repo := &fakeRepo{getItemErr: want}
		svc := newTestService(repo, &fakeCollab{})

		changed, err := svc.CancelWaitlistItem(context.Background(), "wl-1", "cart-1")
		if changed {
			t.Fatalf("changed = true, want false")
		}
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want wrap of %v", err, want)
		}
	})
}

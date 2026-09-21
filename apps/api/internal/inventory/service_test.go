package inventory_test

// Testes puros (sem DB) do inventory.Service. B3a cobre os dois fluxos de baixo
// blast-radius (ListActiveWaitlistByCart, CancelWaitlistItem); B3b cobre o núcleo
// concorrente migrado (ProcessWaitlistForProduct + guardas de ordem, ExpireCart e
// o sweep de 'notified'). Espelham o estilo de fake-port de
// internal/erp/service_test.go: um fakeRepo grava as chamadas e devolve valores/
// erros configuráveis, um fakeCollab conta os callbacks, e o *erp.StockReservations
// é real sobre um repo no-op (só emite eventos best-effort).

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/inventory"
)

// fakeRepo implementa inventory.InventoryRepository por inteiro (B3a + B3b),
// gravando as chamadas e devolvendo valores/erros configuráveis.
type fakeRepo struct {
	promotions   []*inventory.WaitlistPromotion
	promotionErr error

	// B3a
	listRows   []inventory.ListActiveByCartRow
	listErr    error
	item       *inventory.WaitlistItemRow
	getItemErr error
	cancelErr  error
	cart       *inventory.CartRef

	// B3b — promoção
	ttl          time.Duration
	claim        *inventory.WaitlistItemRow
	claimErr     error
	taken        int
	takenErr     error
	lockAcquired bool
	lockErr      error
	snap         *inventory.CartExpirySnapshot
	snapErr      error
	product      *inventory.ProductRef
	productErr   error
	found        bool
	foundErr     error

	// B3b — sweep / expire
	expireResult    inventory.ExpireCartResult
	expireErr       error
	notifiedList    []inventory.WaitlistItemRow
	notifiedListErr error
	updateStatusErr []error // popped per call (nil = success)
	events          []string
	eventsErr       error

	// contadores
	cancelCalls          int
	decCalls             int
	waitlistedDecrements map[string]int
	revertCalls          int
	incrementCalls       int
	lockCalls            int
	requeueCalls         int
	requeueRem           int
	emitNotified         int
	extendCalls          int
	updateCalls          int
	emitExpired          int
	expireRelCalls       int
}

// --- B3a port ---

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

// --- B3b port ---

func (r *fakeRepo) GetWaitlistNotifiedTTL(context.Context, string) (time.Duration, error) {
	if r.ttl == 0 {
		return 30 * time.Minute, nil
	}
	return r.ttl, nil
}
func (r *fakeRepo) ClaimNextWaitlistItem(context.Context, string, string, time.Time) (*inventory.WaitlistItemRow, error) {
	return r.claim, r.claimErr
}
func (r *fakeRepo) DecrementProductStockUpTo(context.Context, string, int) (int, error) {
	return r.taken, r.takenErr
}
func (r *fakeRepo) RevertWaitlistToWaiting(context.Context, string) error {
	r.revertCalls++
	return nil
}
func (r *fakeRepo) IncrementProductStock(context.Context, string, int) error {
	r.incrementCalls++
	return nil
}
func (r *fakeRepo) AcquireCartFinalisationLock(context.Context, string) (func(), bool, error) {
	r.lockCalls++
	if r.lockErr != nil {
		return nil, false, r.lockErr
	}
	if !r.lockAcquired {
		return nil, false, nil
	}
	return func() {}, true, nil
}
func (r *fakeRepo) GetCartExpirySnapshot(context.Context, string) (*inventory.CartExpirySnapshot, error) {
	return r.snap, r.snapErr
}
func (r *fakeRepo) GetProductByID(context.Context, string, string) (*inventory.ProductRef, error) {
	return r.product, r.productErr
}
func (r *fakeRepo) DecrementCartItemWaitlistedQuantity(_ context.Context, cartID, productID string, delta int) (bool, error) {
	if r.waitlistedDecrements == nil {
		r.waitlistedDecrements = map[string]int{}
	}
	r.waitlistedDecrements[cartID+"|"+productID] += delta
	return r.found, r.foundErr
}
func (r *fakeRepo) RequeueWaitlistItemPartial(_ context.Context, _ string, remainingQty int) error {
	r.requeueCalls++
	r.requeueRem = remainingQty
	return nil
}
func (r *fakeRepo) GetCartTokenByID(context.Context, string) (string, error) {
	return "tok", nil
}
func (r *fakeRepo) ExtendCartExpiration(context.Context, string, time.Time) error {
	r.extendCalls++
	return nil
}
func (r *fakeRepo) EmitWaitlistNotified(context.Context, inventory.EmitWaitlistNotifiedParams) error {
	r.emitNotified++
	return nil
}
func (r *fakeRepo) ExpireCartAndReleaseStock(context.Context, string, string) (inventory.ExpireCartResult, error) {
	r.expireRelCalls++
	return r.expireResult, r.expireErr
}
func (r *fakeRepo) UpdateWaitlistItemStatus(context.Context, string, string, *time.Time, *time.Time, *time.Time) error {
	r.updateCalls++
	if len(r.updateStatusErr) == 0 {
		return nil
	}
	err := r.updateStatusErr[0]
	r.updateStatusErr = r.updateStatusErr[1:]
	return err
}
func (r *fakeRepo) EmitWaitlistExpired(context.Context, string, string, string, string) error {
	r.emitExpired++
	return nil
}
func (r *fakeRepo) ListExpiredNotifiedWaitlist(context.Context) ([]inventory.WaitlistItemRow, error) {
	return r.notifiedList, r.notifiedListErr
}
func (r *fakeRepo) GetProductIDByExternalID(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (r *fakeRepo) HasInFlightFinalisationForProduct(context.Context, string) (bool, error) {
	return false, nil
}
func (r *fakeRepo) ListEventsWithWaitingByProduct(context.Context, string) ([]string, error) {
	return r.events, r.eventsErr
}

// fakeCollab implementa inventory.WaitlistCollaborators.
type fakeCollab struct {
	adjustCalls  int
	adjustDelta  int
	reserveCalls int
	scheduleN    int
	notifyN      int
}

func (c *fakeCollab) AdjustStockReservationDelta(_ context.Context, _, _, _, _ string, delta int, _ int64, _ string, _ erp.StockOp) (string, error) {
	c.adjustCalls++
	c.adjustDelta = delta
	return "mov-1", nil
}
func (c *fakeCollab) ReserveStockInERP(context.Context, string, string, string, string, int, int64, string) error {
	c.reserveCalls++
	return nil
}
func (c *fakeCollab) ScheduleExpiry(context.Context, string) error {
	c.scheduleN++
	return nil
}
func (c *fakeCollab) NotifyWaitlistPromoted(context.Context, inventory.WaitlistNotifiedInput) {
	c.notifyN++
}

// noopStockRepo satisfies the (unexported) erp stock repo so we can build a real
// *erp.StockReservations — the flows under test that reach the success point call
// NoteReserved (which only emits a best-effort event), never Release.
type noopStockRepo struct{}

func (noopStockRepo) IncrementProductStock(context.Context, string, int) error      { return nil }
func (noopStockRepo) EmitStockReserved(context.Context, erp.StockEventParams) error { return nil }
func (noopStockRepo) EmitStockReleased(context.Context, erp.StockEventParams) error { return nil }

func newTestService(repo *fakeRepo, collab *fakeCollab) *inventory.Service {
	stock := erp.NewStockReservations(noopStockRepo{}, zap.NewNop())
	// live=nil: the scenarios under test keep found=true (or assert the defensive
	// nil-guard), so the AddToCart fallback is never exercised against a real one.
	return inventory.NewService(repo, collab, stock, nil, zap.NewNop())
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

func (r *fakeRepo) PromoteNextWaitlistEntry(context.Context, string, string) (*inventory.WaitlistPromotion, error) {
	if r.promotionErr != nil {
		return nil, r.promotionErr
	}
	if len(r.promotions) == 0 {
		return nil, nil
	}
	next := r.promotions[0]
	r.promotions = r.promotions[1:]
	return next, nil
}
func (r *fakeRepo) CancelWaitingRequest(_ context.Context, _ string, cartID string) (bool, error) {
	if r.getItemErr != nil {
		return false, r.getItemErr
	}
	if r.cancelErr != nil {
		return false, r.cancelErr
	}
	if r.item == nil || r.item.Status != "waiting" {
		return false, nil
	}
	r.cancelCalls++
	if r.waitlistedDecrements == nil {
		r.waitlistedDecrements = map[string]int{}
	}
	r.waitlistedDecrements[cartID+"|"+r.item.ProductID] += r.item.Quantity
	return true, nil
}

func TestService_CancelWaitlistItem(t *testing.T) {
	for _, status := range []string{"waiting", "notified", "fulfilled", "expired", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			repo := &fakeRepo{item: &inventory.WaitlistItemRow{Status: status, Quantity: 2}}
			collab := &fakeCollab{}
			changed, err := newTestService(repo, collab).CancelWaitlistItem(context.Background(), "w1", "c1")
			if err != nil || changed != (status == "waiting") {
				t.Fatalf("changed=%v error=%v", changed, err)
			}
			if collab.adjustCalls != 0 || repo.decCalls != 0 {
				t.Fatal("leaving queue changed reserved stock")
			}
		})
	}
}

func TestService_ProcessWaitlistForProduct(t *testing.T) {
	t.Run("drains committed allocations without changing deadlines or sending a false DM", func(t *testing.T) {
		repo := &fakeRepo{promotions: []*inventory.WaitlistPromotion{
			{CartID: "first", EventID: "old-event", Quantity: 1, UnitPrice: 1000},
			{CartID: "second", EventID: "new-event", Quantity: 2, UnitPrice: 2000},
		}}
		collab := &fakeCollab{}
		err := newTestService(repo, collab).ProcessWaitlistProduct(context.Background(), "s1", "p1")
		if err != nil {
			t.Fatal(err)
		}
		if collab.reserveCalls != 2 {
			t.Fatalf("ERP calls=%d want2", collab.reserveCalls)
		}
		if repo.extendCalls != 0 || collab.scheduleN != 0 || collab.notifyN != 0 {
			t.Fatal("promotion changed deadline or claimed a notification")
		}
	})
	t.Run("durable caller gets contention errors for retry", func(t *testing.T) {
		repo := &fakeRepo{promotionErr: inventory.ErrWaitlistPromotionDeferred}
		err := newTestService(repo, &fakeCollab{}).ProcessWaitlistProduct(context.Background(), "s1", "p1")
		if !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestService_ExpireCart(t *testing.T) {
	t.Run("lock not acquired is a no-op (finalisation in progress)", func(t *testing.T) {
		repo := &fakeRepo{lockAcquired: false}
		newTestService(repo, &fakeCollab{}).ExpireCart(context.Background(), "cart-1", "s1")

		if repo.expireRelCalls != 0 {
			t.Fatalf("expireRelCalls=%d, want 0 (must not flip while payment finalises)", repo.expireRelCalls)
		}
	})

	t.Run("ineligible cart flips nothing further", func(t *testing.T) {
		repo := &fakeRepo{lockAcquired: true, expireResult: inventory.ExpireCartResult{Eligible: false}}
		newTestService(repo, &fakeCollab{}).ExpireCart(context.Background(), "cart-1", "s1")

		if repo.expireRelCalls != 1 {
			t.Fatalf("expireRelCalls=%d, want 1", repo.expireRelCalls)
		}
	})
}

func TestService_ExpireNotifiedWaitlistSweep(t *testing.T) {
	repo := &fakeRepo{notifiedList: []inventory.WaitlistItemRow{{ID: "old", Status: "notified"}}}
	collab := &fakeCollab{}
	svc := newTestService(repo, collab)
	count, err := svc.ExpireNotifiedWaitlistSweep(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
	if err := svc.ExpireNotifiedWaitlistItem(context.Background(), repo.notifiedList[0]); err != nil {
		t.Fatal(err)
	}
	if repo.decCalls != 0 || repo.updateCalls != 0 || collab.adjustCalls != 0 {
		t.Fatal("legacy expiry job mutated a promoted item")
	}
}

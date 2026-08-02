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
	cancelCalls    int
	decCalls       int
	revertCalls    int
	incrementCalls int
	lockCalls      int
	requeueCalls   int
	requeueRem     int
	emitNotified   int
	extendCalls    int
	updateCalls    int
	emitExpired    int
	expireRelCalls int
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
func (r *fakeRepo) DecrementCartItemWaitlistedQuantity(context.Context, string, string, int) (bool, error) {
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

func TestService_CancelWaitlistItem(t *testing.T) {
	t.Run("notified with cart reverses reservation and promotes queue", func(t *testing.T) {
		repo := &fakeRepo{
			item: &inventory.WaitlistItemRow{Status: "notified", ProductID: "p1", Quantity: 3, EventID: "e1"},
			cart: &inventory.CartRef{StoreID: "s1", PlatformHandle: "buyer"},
			// promotion after cancel finds an empty queue → no-op
			claim: nil,
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
	})

	t.Run("waiting only cancels, no stock movement", func(t *testing.T) {
		repo := &fakeRepo{item: &inventory.WaitlistItemRow{Status: "waiting", ProductID: "p1", Quantity: 1}}
		collab := &fakeCollab{}
		svc := newTestService(repo, collab)

		changed, err := svc.CancelWaitlistItem(context.Background(), "wl-1", "cart-1")
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want true nil", changed, err)
		}
		if repo.cancelCalls != 1 || repo.decCalls != 0 || collab.adjustCalls != 0 {
			t.Fatalf("unexpected side effects: cancel=%d dec=%d adjust=%d",
				repo.cancelCalls, repo.decCalls, collab.adjustCalls)
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

// baseClaim returns a fully-populated claimed row for the promotion scenarios.
func baseClaim(qty int) *inventory.WaitlistItemRow {
	return &inventory.WaitlistItemRow{
		ID: "wl-1", EventID: "e1", ProductID: "p1", CartID: "cart-1",
		PlatformUserID: "u1", PlatformHandle: "buyer", Quantity: qty, Status: "notified",
	}
}

func TestService_ProcessWaitlistForProduct(t *testing.T) {
	t.Run("empty queue is a no-op", func(t *testing.T) {
		repo := &fakeRepo{claim: nil}
		collab := &fakeCollab{}
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.lockCalls != 0 || repo.emitNotified != 0 {
			t.Fatalf("lockCalls=%d emitNotified=%d, want 0 0", repo.lockCalls, repo.emitNotified)
		}
	})

	t.Run("stock gate miss reverts WITHOUT touching the lock", func(t *testing.T) {
		repo := &fakeRepo{claim: baseClaim(2), taken: 0}
		collab := &fakeCollab{}
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.lockCalls != 0 {
			t.Fatalf("lockCalls=%d, want 0 (gate must run before the lock)", repo.lockCalls)
		}
		if repo.revertCalls != 1 || repo.incrementCalls != 0 {
			t.Fatalf("revertCalls=%d incrementCalls=%d, want 1 0 (nothing was taken)", repo.revertCalls, repo.incrementCalls)
		}
		if repo.emitNotified != 0 {
			t.Fatalf("emitNotified=%d, want 0", repo.emitNotified)
		}
	})

	t.Run("lock contention returns the unit and reverts the claim", func(t *testing.T) {
		repo := &fakeRepo{claim: baseClaim(1), taken: 1, lockAcquired: false}
		collab := &fakeCollab{}
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.lockCalls == 0 {
			t.Fatalf("lockCalls=0, want the retry to have attempted the lock")
		}
		if repo.incrementCalls != 1 || repo.revertCalls != 1 {
			t.Fatalf("incrementCalls=%d revertCalls=%d, want 1 1 (unit returned, buyer kept)", repo.incrementCalls, repo.revertCalls)
		}
		if repo.emitNotified != 0 || collab.notifyN != 0 {
			t.Fatalf("emitNotified=%d notifyN=%d, want 0 0 (no promotion)", repo.emitNotified, collab.notifyN)
		}
	})

	t.Run("terminal cart under the lock defers the buyer", func(t *testing.T) {
		repo := &fakeRepo{
			claim: baseClaim(1), taken: 1, lockAcquired: true,
			snap: &inventory.CartExpirySnapshot{Status: "active", PaymentStatus: "paid"}, // terminal
		}
		collab := &fakeCollab{}
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.incrementCalls != 1 || repo.revertCalls != 1 {
			t.Fatalf("incrementCalls=%d revertCalls=%d, want 1 1 (deferred, unit returned)", repo.incrementCalls, repo.revertCalls)
		}
		if repo.emitNotified != 0 {
			t.Fatalf("emitNotified=%d, want 0 (no promotion on terminal cart)", repo.emitNotified)
		}
	})

	t.Run("full promotion emits NoteReserved+EmitWaitlistNotified exactly once", func(t *testing.T) {
		repo := &fakeRepo{
			claim: baseClaim(2), taken: 2, lockAcquired: true,
			snap:    &inventory.CartExpirySnapshot{Status: "active", PaymentStatus: "pending"},
			product: &inventory.ProductRef{Price: 1000, Name: "Camisa", Keyword: "cam"},
			found:   true,
		}
		collab := &fakeCollab{}
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.emitNotified != 1 {
			t.Fatalf("emitNotified=%d, want 1", repo.emitNotified)
		}
		if repo.incrementCalls != 0 || repo.revertCalls != 0 {
			t.Fatalf("incrementCalls=%d revertCalls=%d, want 0 0 (no rollback on success)", repo.incrementCalls, repo.revertCalls)
		}
		if repo.requeueCalls != 0 {
			t.Fatalf("requeueCalls=%d, want 0 (full promotion)", repo.requeueCalls)
		}
		if collab.reserveCalls != 1 || collab.scheduleN != 1 || collab.notifyN != 1 {
			t.Fatalf("reserve=%d schedule=%d notify=%d, want 1 1 1", collab.reserveCalls, collab.scheduleN, collab.notifyN)
		}
	})

	t.Run("partial promotion requeues the remainder", func(t *testing.T) {
		repo := &fakeRepo{
			claim: baseClaim(3), taken: 2, lockAcquired: true,
			snap:    &inventory.CartExpirySnapshot{Status: "active", PaymentStatus: "pending"},
			product: &inventory.ProductRef{Price: 1000, Name: "Camisa", Keyword: "cam"},
			found:   true,
		}
		collab := &fakeCollab{}
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.requeueCalls != 1 || repo.requeueRem != 1 {
			t.Fatalf("requeueCalls=%d requeueRem=%d, want 1 and 1", repo.requeueCalls, repo.requeueRem)
		}
		if repo.emitNotified != 1 {
			t.Fatalf("emitNotified=%d, want 1", repo.emitNotified)
		}
	})

	t.Run("missing cart item with no live service reverts defensively", func(t *testing.T) {
		repo := &fakeRepo{
			claim: baseClaim(1), taken: 1, lockAcquired: true,
			snap:    &inventory.CartExpirySnapshot{Status: "active", PaymentStatus: "pending"},
			product: &inventory.ProductRef{Price: 1000, Name: "Camisa", Keyword: "cam"},
			found:   false, // cart item was deleted → fallback path
		}
		collab := &fakeCollab{}
		// newTestService injects live=nil → the defensive nil-guard must revert.
		newTestService(repo, collab).ProcessWaitlistForProduct(context.Background(), "e1", "p1", "s1")

		if repo.incrementCalls != 1 || repo.revertCalls != 1 {
			t.Fatalf("incrementCalls=%d revertCalls=%d, want 1 1 (defensive revert)", repo.incrementCalls, repo.revertCalls)
		}
		if repo.emitNotified != 0 || collab.notifyN != 0 {
			t.Fatalf("emitNotified=%d notifyN=%d, want 0 0 (no promotion)", repo.emitNotified, collab.notifyN)
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
	t.Run("continues after a per-item error and reports the first", func(t *testing.T) {
		boom := errors.New("update failed")
		repo := &fakeRepo{
			// CartID empty → skip the cart branch, isolating the status-flip gate.
			notifiedList: []inventory.WaitlistItemRow{
				{ID: "a", EventID: "e1", ProductID: "p1"},
				{ID: "b", EventID: "e1", ProductID: "p1"},
			},
			updateStatusErr: []error{boom, nil}, // first item fails, second succeeds
		}
		processed, err := newTestService(repo, &fakeCollab{}).ExpireNotifiedWaitlistSweep(context.Background())

		if processed != 1 {
			t.Fatalf("processed=%d, want 1 (second item still handled)", processed)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("err=%v, want wrap of %v", err, boom)
		}
		if repo.updateCalls != 2 {
			t.Fatalf("updateCalls=%d, want 2 (sweep did not stop at the first error)", repo.updateCalls)
		}
	})

	t.Run("list error is wrapped", func(t *testing.T) {
		want := errors.New("db down")
		repo := &fakeRepo{notifiedListErr: want}
		if _, err := newTestService(repo, &fakeCollab{}).ExpireNotifiedWaitlistSweep(context.Background()); !errors.Is(err, want) {
			t.Fatalf("err=%v, want %v", err, want)
		}
	})
}

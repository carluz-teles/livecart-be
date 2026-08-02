package erp

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// --- test doubles for the Design C state machine ----------------------------

// lifecycleProvider is a scripted ERPProvider for the confirm/mutate paths. It
// embeds the interface (nil) so any unscripted call panics — proving the confirm
// path touches ONLY payment + situação, never stock.
type lifecycleProvider struct {
	providers.ERPProvider
	updatePaymentErr error
	situacaoErr      error
	payments         int
	situacoes        []int
	orderReverses    int
	orderUpdates     int
	orderLaunches    int
}

func (p *lifecycleProvider) UpdateOrderPayment(context.Context, string, *providers.ERPOrderPayment) error {
	p.payments++
	return p.updatePaymentErr
}
func (p *lifecycleProvider) SetOrderSituacao(_ context.Context, _ string, situacao int) error {
	p.situacoes = append(p.situacoes, situacao)
	return p.situacaoErr
}
func (p *lifecycleProvider) ReverseOrderStock(context.Context, string) error {
	p.orderReverses++
	return nil
}
func (p *lifecycleProvider) UpdateOrderItems(context.Context, string, []providers.ERPOrderItem) error {
	p.orderUpdates++
	return nil
}
func (p *lifecycleProvider) LaunchOrderStock(context.Context, string) error {
	p.orderLaunches++
	return nil
}

// lifecycleRepo is a controllable ERPRepository for the state machine. State is
// held in a pointer so transitions mutate it (CAS is modelled as always-win).
type lifecycleRepo struct {
	stubERPRepo // no-op base; we override what the confirm/mutate paths call

	state       *CartERPOrderState
	items       []NonWaitlistedCartItem
	lockAcq     bool
	transitions []string // "from->to"
	doneMarked  bool
}

func (r *lifecycleRepo) AcquireCartFinalisationLock(context.Context, string) (func(), bool, error) {
	return func() {}, r.lockAcq, nil
}
func (r *lifecycleRepo) GetCartERPOrderState(context.Context, string) (*CartERPOrderState, error) {
	return r.state, nil
}
func (r *lifecycleRepo) GetActiveByProvider(context.Context, string, string, string) (*Integration, error) {
	return &Integration{ID: "int-1"}, nil
}
func (r *lifecycleRepo) TransitionCartERPOrderState(_ context.Context, _, from, to string) (bool, error) {
	r.transitions = append(r.transitions, from+"->"+to)
	r.state.State = to
	return true, nil
}
func (r *lifecycleRepo) MarkCartERPFinalisationDone(context.Context, string) error {
	r.doneMarked = true
	return nil
}
func (r *lifecycleRepo) ListNonWaitlistedCartItems(context.Context, string) ([]NonWaitlistedCartItem, error) {
	return r.items, nil
}

// lifecycleCollab records the best-effort side-effect calls.
type lifecycleCollab struct {
	stubCollaborators
	provider  providers.ERPProvider
	finalized int
	failed    int
	mirrors   int
}

func (c *lifecycleCollab) ResolveProvider(context.Context, *Integration) (providers.ERPProvider, error) {
	return c.provider, nil
}
func (c *lifecycleCollab) EmitERPOrderFinalized(context.Context, string, string)  { c.finalized++ }
func (c *lifecycleCollab) MarkFinalisationFailed(context.Context, string, string) { c.failed++ }
func (c *lifecycleCollab) MirrorToOrder(context.Context, string)                  { c.mirrors++ }

// --- ConfirmERPOrderPayment -------------------------------------------------

func TestService_ConfirmERPOrderPayment(t *testing.T) {
	ctx := context.Background()

	t.Run("open cart confirms with two writes and zero stock movement", func(t *testing.T) {
		prov := &lifecycleProvider{}
		repo := &lifecycleRepo{
			state:   &CartERPOrderState{State: OrderStateOpen, ExternalOrderID: "ORD-1"},
			items:   []NonWaitlistedCartItem{{ProductExternalID: "ext-1", Quantity: 2, UnitPrice: 1000}},
			lockAcq: true,
		}
		collab := &lifecycleCollab{provider: prov}
		svc := NewService(repo, collab, zap.NewNop())

		if err := svc.ConfirmERPOrderPayment(ctx, "c", "s", testStatus()); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if prov.payments != 1 {
			t.Fatalf("expected one payment write, got %d", prov.payments)
		}
		if len(prov.situacoes) != 1 || prov.situacoes[0] != 3 {
			t.Fatalf("expected a single situação=3 (Aprovada), got %v", prov.situacoes)
		}
		if prov.orderReverses != 0 || prov.orderLaunches != 0 {
			t.Fatalf("confirm must not move stock: reverses=%d launches=%d", prov.orderReverses, prov.orderLaunches)
		}
		if repo.state.State != OrderStateConfirmed || !repo.doneMarked {
			t.Fatalf("cart must land confirmed+done, got state=%q done=%v", repo.state.State, repo.doneMarked)
		}
		if collab.finalized != 1 || collab.mirrors != 1 {
			t.Fatalf("expected finalized+mirror facts, got finalized=%d mirrors=%d", collab.finalized, collab.mirrors)
		}
	})

	t.Run("lock lost is a silent no-op (duplicate webhook)", func(t *testing.T) {
		repo := &lifecycleRepo{state: &CartERPOrderState{State: OrderStateOpen}, lockAcq: false}
		collab := &lifecycleCollab{provider: &lifecycleProvider{}}
		svc := NewService(repo, collab, zap.NewNop())

		if err := svc.ConfirmERPOrderPayment(ctx, "c", "s", testStatus()); err != nil {
			t.Fatalf("duplicate trigger should no-op, got %v", err)
		}
		if len(repo.transitions) != 0 {
			t.Fatalf("lock loser must not touch the state machine, got %v", repo.transitions)
		}
	})

	t.Run("unconverted cart falls back to legacy via ErrCartNotConverted", func(t *testing.T) {
		repo := &lifecycleRepo{state: &CartERPOrderState{State: OrderStateNone}, lockAcq: true}
		collab := &lifecycleCollab{provider: &lifecycleProvider{}}
		svc := NewService(repo, collab, zap.NewNop())

		err := svc.ConfirmERPOrderPayment(ctx, "c", "s", testStatus())
		if !errors.Is(err, ErrCartNotConverted) {
			t.Fatalf("want ErrCartNotConverted, got %v", err)
		}
	})

	t.Run("approval failure marks finalisation failed and propagates", func(t *testing.T) {
		prov := &lifecycleProvider{situacaoErr: errors.New("tiny 500")}
		repo := &lifecycleRepo{
			state:   &CartERPOrderState{State: OrderStateOpen, ExternalOrderID: "ORD-1"},
			items:   []NonWaitlistedCartItem{{ProductExternalID: "ext-1", Quantity: 1, UnitPrice: 1000}},
			lockAcq: true,
		}
		collab := &lifecycleCollab{provider: prov}
		svc := NewService(repo, collab, zap.NewNop())

		err := svc.ConfirmERPOrderPayment(ctx, "c", "s", testStatus())
		if err == nil {
			t.Fatal("expected the approval error to propagate")
		}
		if collab.failed != 1 {
			t.Fatalf("approval failure must mark finalisation failed once, got %d", collab.failed)
		}
		if repo.state.State == OrderStateConfirmed {
			t.Fatal("cart must not land confirmed when approval failed")
		}
	})
}

// --- MutateERPOrderItems ----------------------------------------------------

func TestService_MutateERPOrderItems(t *testing.T) {
	ctx := context.Background()

	t.Run("cart not open returns ErrCartNotConverted", func(t *testing.T) {
		repo := &lifecycleRepo{state: &CartERPOrderState{State: OrderStateNone}}
		collab := &lifecycleCollab{provider: &lifecycleProvider{}}
		svc := NewService(repo, collab, zap.NewNop())

		err := svc.MutateERPOrderItems(ctx, "c", "s")
		if !errors.Is(err, ErrCartNotConverted) {
			t.Fatalf("want ErrCartNotConverted, got %v", err)
		}
	})

	t.Run("open cart runs estornar→PUT→lançar and returns to open", func(t *testing.T) {
		prov := &lifecycleProvider{}
		repo := &lifecycleRepo{
			state: &CartERPOrderState{State: OrderStateOpen, ExternalOrderID: "ORD-1"},
			items: []NonWaitlistedCartItem{{ProductExternalID: "ext-1", Quantity: 1, UnitPrice: 1000}},
		}
		collab := &lifecycleCollab{provider: prov}
		svc := NewService(repo, collab, zap.NewNop())

		if err := svc.MutateERPOrderItems(ctx, "c", "s"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if prov.orderReverses != 1 || prov.orderUpdates != 1 || prov.orderLaunches != 1 {
			t.Fatalf("expected one full estornar→PUT→lançar cycle, got rev=%d put=%d launch=%d",
				prov.orderReverses, prov.orderUpdates, prov.orderLaunches)
		}
		// open→mutating then the deferred mutating→open.
		if repo.state.State != OrderStateOpen {
			t.Fatalf("cart must return to open, got %q", repo.state.State)
		}
	})
}

func testStatus() *providers.PaymentStatus {
	return &providers.PaymentStatus{PaymentID: "PAY-1", PaymentMethod: "pix", Amount: 2000}
}

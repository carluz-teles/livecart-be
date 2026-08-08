package erp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// --- test doubles -----------------------------------------------------------

// fakeERPProvider embeds the interface so it satisfies providers.ERPProvider
// with zero boilerplate; only the two methods the stock flow calls are real.
// Any other call would hit the nil embedded interface and panic, which is the
// point — it proves the flow touches nothing else.
type fakeERPProvider struct {
	providers.ERPProvider
	reserveID  string
	reserveErr error
	reverseID  string
	reverseErr error
	reserves   int
	reverses   int

	// order-as-reservation cycle (used by MutateERPOrderItems, now internal).
	orderReverses int
	orderUpdates  int
	orderLaunches int
}

func (f *fakeERPProvider) ReserveStock(_ context.Context, _ string, _ int, _ float64, _ string) (string, error) {
	f.reserves++
	return f.reserveID, f.reserveErr
}

func (f *fakeERPProvider) ReverseStockReservation(_ context.Context, _ string, _ int, _ float64, _ string) (string, error) {
	f.reverses++
	return f.reverseID, f.reverseErr
}

func (f *fakeERPProvider) ReverseOrderStock(context.Context, string) error {
	f.orderReverses++
	return nil
}
func (f *fakeERPProvider) UpdateOrderItems(context.Context, string, []providers.ERPOrderItem) error {
	f.orderUpdates++
	return nil
}
func (f *fakeERPProvider) LaunchOrderStock(context.Context, string) error {
	f.orderLaunches++
	return nil
}

// mockRepo is a controllable ERPRepository recording the calls the assertions care about.
type mockRepo struct {
	integration   *Integration
	getActiveErr  error
	orderState    *CartERPOrderState
	orderStateErr error
	existing      []StockReservationRow
	items         []NonWaitlistedCartItem
	decrementErr  error
	incrementErr  error
	createErr     error

	reserved    int
	released    int
	decrements  int
	increments  int
	creates     int
	adjusts     int
	reverses    int
	transitions int

	// Cart NFe sync (Bloco B2d).
	anchorStoreID  string
	anchorExtOrder string
	anchorErr      error
	upsertRows     int64
	upsertErr      error
	findCartID     string
	findErr        error
	shipment       *ShipmentInvoiceRef
	upserts        int
	invoiceMirrors int
}

func (m *mockRepo) GetActiveByProvider(context.Context, string, string, string) (*Integration, error) {
	if m.getActiveErr != nil {
		return nil, m.getActiveErr
	}
	if m.integration != nil {
		return m.integration, nil
	}
	return &Integration{ID: "int-1"}, nil
}
func (m *mockRepo) AcquireCartFinalisationLock(context.Context, string) (func(), bool, error) {
	return func() {}, false, nil
}
func (m *mockRepo) GetCartERPOrderState(context.Context, string) (*CartERPOrderState, error) {
	if m.orderStateErr != nil {
		return nil, m.orderStateErr
	}
	if m.orderState != nil {
		return m.orderState, nil
	}
	return &CartERPOrderState{State: OrderStateNone}, nil
}
func (m *mockRepo) ListActiveReservationsByCartAndProduct(context.Context, string, string) ([]StockReservationRow, error) {
	return m.existing, nil
}
func (m *mockRepo) CreateStockReservation(context.Context, CreateStockReservationParams) (*StockReservationRow, error) {
	m.creates++
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &StockReservationRow{}, nil
}
func (m *mockRepo) AdjustActiveReservationQuantity(context.Context, string, string, int, string) (*StockReservationRow, error) {
	m.adjusts++
	return &StockReservationRow{}, nil
}
func (m *mockRepo) ReverseReservationsByCartAndProduct(context.Context, string, string) error {
	m.reverses++
	return nil
}
func (m *mockRepo) DecrementProductStock(context.Context, string, int) error {
	m.decrements++
	return m.decrementErr
}
func (m *mockRepo) IncrementProductStock(context.Context, string, int) error {
	m.increments++
	return m.incrementErr
}
func (m *mockRepo) EmitStockReserved(context.Context, StockEventParams) error {
	m.reserved++
	return nil
}
func (m *mockRepo) EmitStockReleased(context.Context, StockEventParams) error {
	m.released++
	return nil
}
func (m *mockRepo) TransitionCartERPOrderState(context.Context, string, string, string) (bool, error) {
	m.transitions++
	return true, nil
}
func (m *mockRepo) UpdateCartExternalOrderID(context.Context, string, string) error { return nil }
func (m *mockRepo) SetCartERPStockLaunched(context.Context, string, bool) error     { return nil }
func (m *mockRepo) MarkCartERPFinalisationDone(context.Context, string) error       { return nil }
func (m *mockRepo) ListNonWaitlistedCartItems(context.Context, string) ([]NonWaitlistedCartItem, error) {
	return m.items, nil
}
func (m *mockRepo) ListStuckERPOrderOps(context.Context, time.Duration) ([]StuckERPOrderOp, error) {
	return nil, nil
}
func (m *mockRepo) ListTinyIntegrationsWithStaleStockWebhook(context.Context, time.Duration) ([]StaleStockWebhookIntegration, error) {
	return nil, nil
}
func (m *mockRepo) StampIntegrationStockWebhookAlert(context.Context, string) error { return nil }
func (m *mockRepo) GetByProvider(context.Context, string, string, string) (*Integration, error) {
	return nil, nil
}
func (m *mockRepo) GetCartERPFinalisationStatus(context.Context, string) (*CartFinalisationStatus, error) {
	return nil, nil
}
func (m *mockRepo) MarkCartERPFinalisationAttempt(context.Context, string, []byte) error { return nil }
func (m *mockRepo) ListActiveReservationsByCart(context.Context, string) ([]StockReservationRow, error) {
	return nil, nil
}
func (m *mockRepo) ReverseReservationByID(context.Context, string) error    { return nil }
func (m *mockRepo) ReverseReservationsByCart(context.Context, string) error { return nil }
func (m *mockRepo) ClaimReservationForReversal(context.Context, string) (bool, error) {
	return true, nil
}
func (m *mockRepo) RestoreReservationToActive(context.Context, string) error { return nil }
func (m *mockRepo) GetCartInvoiceAnchor(context.Context, string) (string, string, error) {
	return m.anchorStoreID, m.anchorExtOrder, m.anchorErr
}
func (m *mockRepo) UpsertCartERPInvoice(context.Context, UpsertCartERPInvoiceParams) (int64, error) {
	m.upserts++
	return m.upsertRows, m.upsertErr
}
func (m *mockRepo) FindCartByExternalOrderID(context.Context, string, string) (string, error) {
	return m.findCartID, m.findErr
}
func (m *mockRepo) GetShipmentByOrderID(context.Context, string) (*ShipmentInvoiceRef, error) {
	return m.shipment, nil
}
func (m *mockRepo) UpdateShipmentInvoice(context.Context, string, string, string) error {
	m.invoiceMirrors++
	return nil
}

// mockCollab is a controllable StockCollaborators.
type mockCollab struct {
	provider    providers.ERPProvider
	providerErr error
	linked      bool
	externalID  string

	// Cart NFe sync / health-check (Bloco B2d).
	providerByID    providers.ERPProvider
	providerByIDErr error
	handledErrors   int
}

func (m *mockCollab) ResolveProvider(context.Context, *Integration) (providers.ERPProvider, error) {
	return m.provider, m.providerErr
}
func (m *mockCollab) ResolveExternalProduct(context.Context, string, string) (string, bool) {
	return m.externalID, m.linked
}
func (m *mockCollab) OrderAtCheckoutEnabled(string) bool { return false }
func (m *mockCollab) ResolveERPContact(context.Context, providers.ERPProvider, *Integration, string, string, string, string, string, string, string) (string, error) {
	return "", nil
}
func (m *mockCollab) CreateFinalERPOrderForConversion(context.Context, providers.ERPProvider, *Integration, string, string) error {
	return nil
}
func (m *mockCollab) CreateFinalERPOrder(context.Context, providers.ERPProvider, *Integration, string, string, *providers.PaymentStatus, bool) error {
	return nil
}
func (m *mockCollab) FinalisationInverted(string) bool { return false }
func (m *mockCollab) ReReserveAfterFailedFinalisation(context.Context, providers.ERPProvider, string, []StockReservationRow) {
}
func (m *mockCollab) ReverseCartReservationsPerRow(context.Context, providers.ERPProvider, string) error {
	return nil
}
func (m *mockCollab) MarkFinalisationFailed(context.Context, string, string)                {}
func (m *mockCollab) MirrorToOrder(context.Context, string)                                 {}
func (m *mockCollab) EmitERPOrderFinalized(context.Context, string, string)                 {}
func (m *mockCollab) EmitERPOrderCancelled(context.Context, string, string, string, string) {}
func (m *mockCollab) ResolveERPProviderByID(context.Context, string, string) (providers.ERPProvider, error) {
	return m.providerByID, m.providerByIDErr
}
func (m *mockCollab) HandleProviderError(context.Context, string, string, error) {
	m.handledErrors++
}

func newSvc(repo *mockRepo, collab *mockCollab) *Service {
	return NewService(repo, collab, zap.NewNop())
}

// --- ReserveStockInERP ------------------------------------------------------

func TestService_ReserveStockInERP(t *testing.T) {
	ctx := context.Background()

	t.Run("no ERP integration is a silent no-op", func(t *testing.T) {
		repo := &mockRepo{getActiveErr: errors.New("no integration")}
		collab := &mockCollab{}
		if err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 1, 1000, "@h"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if repo.creates != 0 || repo.transitions != 0 {
			t.Fatalf("no-op should touch nothing: creates=%d transitions=%d", repo.creates, repo.transitions)
		}
	})

	t.Run("converted cart routes to the order cycle, no manual movement", func(t *testing.T) {
		fake := &fakeERPProvider{}
		repo := &mockRepo{
			orderState: &CartERPOrderState{State: OrderStateOpen, ExternalOrderID: "ORD-1"},
			items:      []NonWaitlistedCartItem{{ProductExternalID: "ext-1", Quantity: 1, UnitPrice: 1000}},
		}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		if err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 1, 1000, "@h"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		// The mutation now runs internally (MutateERPOrderItems moved to erp.Service):
		// it drives the estornar→PUT→lançar cycle on the order, not a manual exit.
		if fake.orderUpdates != 1 {
			t.Fatalf("expected one order-items PUT, got %d", fake.orderUpdates)
		}
		if fake.reserves != 0 || repo.creates != 0 {
			t.Fatalf("manual movement leaked into converted cart: reserves=%d creates=%d", fake.reserves, repo.creates)
		}
	})

	t.Run("product not linked is a silent no-op", func(t *testing.T) {
		fake := &fakeERPProvider{}
		repo := &mockRepo{}
		collab := &mockCollab{provider: fake, linked: false}
		if err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 1, 1000, "@h"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if fake.reserves != 0 || repo.creates != 0 {
			t.Fatalf("unlinked product should not move stock")
		}
	})

	t.Run("happy path reserves and records", func(t *testing.T) {
		fake := &fakeERPProvider{reserveID: "mov-1"}
		repo := &mockRepo{}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		if err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 2, 1000, "@h"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if fake.reserves != 1 || repo.creates != 1 {
			t.Fatalf("expected one reserve+create, got reserves=%d creates=%d", fake.reserves, repo.creates)
		}
	})

	// Segunda adição do MESMO produto no MESMO carrinho SOMA à reserva.
	//
	// Este subteste cravava o oposto ("idempotente: pula"), e o que ele
	// protegia não era retentativa — era o comprador pedindo mais uma unidade.
	// O `quantity` recebido é a quantidade DESTA adição, o mesmo número que o
	// chamador já descontou do estoque local. Pular deixava o LiveCart com
	// unidade vendida que o ERP continuava oferecendo: em staging, 5 vendidas e
	// 3 seguradas no Tiny.
	//
	// Repetição de verdade não chega aqui: o comentário é deduplicado por
	// platform_comment_id antes de virar item de carrinho.
	t.Run("segunda adicao do mesmo produto soma na reserva", func(t *testing.T) {
		fake := &fakeERPProvider{}
		repo := &mockRepo{existing: []StockReservationRow{{Quantity: 1}}}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		if err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 2, 1000, "@h"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if fake.reserves != 1 {
			t.Errorf("reserves=%d — a unidade adicional nao foi segurada no ERP e segue a venda em outro canal", fake.reserves)
		}
		if repo.adjusts != 1 {
			t.Errorf("adjusts=%d — a linha de reserva nao acompanhou a quantidade do carrinho", repo.adjusts)
		}
		if repo.creates != 0 {
			t.Errorf("creates=%d — deveria SOMAR na reserva existente, nao criar uma segunda", repo.creates)
		}
	})

	// Falha no ERP não pode gravar aumento local: a linha diria que há mais
	// segurado do que o ERP realmente segura.
	t.Run("falha no ERP nao aumenta a reserva local", func(t *testing.T) {
		fake := &fakeERPProvider{reserveErr: errors.New("tiny down")}
		repo := &mockRepo{existing: []StockReservationRow{{Quantity: 1}}}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		if err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 1, 1000, "@h"); err == nil {
			t.Fatal("esperava erro quando o ERP recusa a reserva adicional")
		}
		if repo.adjusts != 0 {
			t.Errorf("adjusts=%d apos falha no ERP", repo.adjusts)
		}
	})

	t.Run("provider reserve error propagates", func(t *testing.T) {
		fake := &fakeERPProvider{reserveErr: errors.New("tiny down")}
		repo := &mockRepo{}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		err := newSvc(repo, collab).ReserveStockInERP(ctx, "s", "c", "e", "p", 1, 1000, "@h")
		if err == nil {
			t.Fatal("expected error")
		}
		if repo.creates != 0 {
			t.Fatalf("failed reserve must not record a reservation")
		}
	})
}

// --- AdjustStockReservationDelta --------------------------------------------

func TestService_AdjustStockReservationDelta(t *testing.T) {
	ctx := context.Background()

	t.Run("zero delta is a no-op", func(t *testing.T) {
		repo := &mockRepo{}
		mov, err := newSvc(repo, &mockCollab{}).AdjustStockReservationDelta(ctx, "s", "c", "e", "p", 0, 1000, "@h", StockOpUnspecified)
		if err != nil || mov != "" {
			t.Fatalf("want no-op, got mov=%q err=%v", mov, err)
		}
		if repo.decrements != 0 || repo.increments != 0 {
			t.Fatalf("zero delta must not touch stock")
		}
	})

	t.Run("positive delta without ERP updates local stock and emits reserved", func(t *testing.T) {
		repo := &mockRepo{getActiveErr: errors.New("no integration")}
		mov, err := newSvc(repo, &mockCollab{}).AdjustStockReservationDelta(ctx, "s", "c", "e", "p", 3, 1000, "@h", StockOpUnspecified)
		if err != nil || mov != "" {
			t.Fatalf("want empty mov + nil err, got mov=%q err=%v", mov, err)
		}
		if repo.decrements != 1 {
			t.Fatalf("expected one local decrement, got %d", repo.decrements)
		}
		if repo.reserved != 1 {
			t.Fatalf("provisional reserve must emit stock.reserved once, got %d", repo.reserved)
		}
	})

	t.Run("negative delta without ERP releases local stock and emits released", func(t *testing.T) {
		repo := &mockRepo{getActiveErr: errors.New("no integration")}
		mov, err := newSvc(repo, &mockCollab{}).AdjustStockReservationDelta(ctx, "s", "c", "e", "p", -2, 0, "@h", StockOpUnspecified)
		if err != nil || mov != "" {
			t.Fatalf("want empty mov + nil err, got mov=%q err=%v", mov, err)
		}
		if repo.increments != 1 {
			t.Fatalf("expected one local increment, got %d", repo.increments)
		}
		if repo.released != 1 {
			t.Fatalf("release must emit stock.released once, got %d", repo.released)
		}
	})

	t.Run("insufficient stock returns 422 and emits nothing", func(t *testing.T) {
		repo := &mockRepo{decrementErr: pgx.ErrNoRows}
		mov, err := newSvc(repo, &mockCollab{}).AdjustStockReservationDelta(ctx, "s", "c", "e", "p", 5, 1000, "@h", StockOpUnspecified)
		if err == nil {
			t.Fatal("expected an error for insufficient stock")
		}
		if mov != "" {
			t.Fatalf("no movement on failure, got %q", mov)
		}
		if repo.reserved != 0 || repo.released != 0 {
			t.Fatalf("failed decrement must not emit any event")
		}
	})

	t.Run("provider failure rolls local stock back and emits nothing", func(t *testing.T) {
		fake := &fakeERPProvider{reserveErr: errors.New("tiny down")}
		repo := &mockRepo{}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		_, err := newSvc(repo, collab).AdjustStockReservationDelta(ctx, "s", "c", "e", "p", 2, 1000, "@h", StockOpUnspecified)
		if err == nil {
			t.Fatal("expected error from provider")
		}
		if repo.decrements != 1 || repo.increments != 1 {
			t.Fatalf("expected decrement then rollback increment, got dec=%d inc=%d", repo.decrements, repo.increments)
		}
		if repo.reserved != 0 || repo.released != 0 {
			t.Fatalf("rolled-back op must stay silent, got reserved=%d released=%d", repo.reserved, repo.released)
		}
	})

	t.Run("converted cart routes to the order cycle", func(t *testing.T) {
		fake := &fakeERPProvider{}
		repo := &mockRepo{
			orderState: &CartERPOrderState{State: OrderStateOpen, ExternalOrderID: "ORD-1"},
			items:      []NonWaitlistedCartItem{{ProductExternalID: "ext-1", Quantity: 1, UnitPrice: 1000}},
		}
		collab := &mockCollab{provider: fake, linked: true, externalID: "ext-1"}
		mov, err := newSvc(repo, collab).AdjustStockReservationDelta(ctx, "s", "c", "e", "p", 1, 1000, "@h", StockOpUnspecified)
		if err != nil || mov != "" {
			t.Fatalf("converted cart yields no manual movement id: mov=%q err=%v", mov, err)
		}
		// The mutation now runs internally (MutateERPOrderItems moved to erp.Service).
		if fake.orderUpdates != 1 {
			t.Fatalf("expected one order-items PUT, got %d", fake.orderUpdates)
		}
		if fake.reserves != 0 {
			t.Fatalf("manual reserve leaked into converted cart")
		}
		// Local stock still moved (waitlist gate), so the provisional emit fires.
		if repo.decrements != 1 || repo.reserved != 1 {
			t.Fatalf("expected local decrement+emit, got dec=%d reserved=%d", repo.decrements, repo.reserved)
		}
	})
}

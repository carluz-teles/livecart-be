package erp

// Duplos mínimos e controláveis, para os testes que exercitam UM caminho por vez
// (NFe, health-check). Os testes de fluxo e de corrida usam os simuladores —
// erpSimulado e repoSimulado —, que reproduzem a semântica medida do ERP; estes
// aqui existem só para injetar uma resposta e conferir uma chamada.

import (
	"context"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// mockRepo is a controllable ERPRepository recording the calls the assertions care about.
type mockRepo struct {
	integration   *Integration
	getActiveErr  error
	orderState    *CartERPOrderState
	orderStateErr error
	existing      []StockReservationRow
	items         []NonWaitlistedCartItem
	cartShortID   int32
	decrementErr  error
	incrementErr  error
	createErr     error

	reserved           int
	released           int
	decrements         int
	increments         int
	creates            int
	adjusts            int
	reservationUpserts int
	reverses           int
	transitions        int

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
func (m *mockRepo) GetCartERPOpAge(context.Context, string) (time.Duration, error) {
	return time.Hour, nil
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

func (m *mockRepo) CreateStockReservation(context.Context, CreateStockReservationParams) (*StockReservationRow, error) {
	m.creates++
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &StockReservationRow{}, nil
}
func (m *mockRepo) UpsertActiveReservationQuantity(_ context.Context, p UpsertReservationParams) (*StockReservationRow, error) {
	m.reservationUpserts++
	return &StockReservationRow{Quantity: p.IncQty}, nil
}

func (m *mockRepo) DecrementActiveReservationQuantity(context.Context, string, string, int) (ReservationDecrement, error) {
	return ReservationDecrement{}, nil
}

func (m *mockRepo) RestoreReservationQuantityByID(context.Context, string, int) error { return nil }

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
func (m *mockRepo) GetCartShortID(context.Context, string) (int32, error) {
	return m.cartShortID, nil
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

func (m *mockCollab) ResolveERPContact(context.Context, providers.ERPProvider, *Integration, string, string, string, string, string, string, string) (string, error) {
	return "", nil
}
func (m *mockCollab) CreateERPOrderForCart(context.Context, providers.ERPProvider, *Integration, string, string) ([]providers.ERPOrderItem, error) {
	return nil, nil
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

// fakeERPProvider é um ERPProvider que não faz nada. A interface embutida é nil
// de propósito: qualquer método que o fluxo chame sem estar declarado aqui
// explode, e é assim que o teste prova que o caminho não toca em mais nada.
type fakeERPProvider struct {
	providers.ERPProvider
}

func (m *mockRepo) ListERPLinkedProductsSample(context.Context, string, int) ([]ERPLinkedProduct, error) {
	return nil, nil
}

func (m *mockRepo) CartIsTerminated(context.Context, string) (bool, error) { return false, nil }

func (m *mockRepo) ListCartGridItems(ctx context.Context, cartID string) ([]NonWaitlistedCartItem, error) {
	return m.ListNonWaitlistedCartItems(ctx, cartID)
}

func (m *mockRepo) CartIsPaid(context.Context, string) (bool, error) { return false, nil }

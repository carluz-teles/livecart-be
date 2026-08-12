package erp

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// stubERPRepo is a no-op ERPRepository proving the port is implementable without
// a real repository — the point of declaring it on the consumer side. As the
// interface grows in later slices, this stub must grow with it or the build breaks.
type stubERPRepo struct{}

func (stubERPRepo) GetActiveByProvider(context.Context, string, string, string) (*Integration, error) {
	return nil, nil
}
func (stubERPRepo) GetByProvider(context.Context, string, string, string) (*Integration, error) {
	return nil, nil
}
func (stubERPRepo) GetCartERPFinalisationStatus(context.Context, string) (*CartFinalisationStatus, error) {
	return nil, nil
}
func (stubERPRepo) MarkCartERPFinalisationAttempt(context.Context, string, []byte) error { return nil }
func (stubERPRepo) ListActiveReservationsByCart(context.Context, string) ([]StockReservationRow, error) {
	return nil, nil
}
func (stubERPRepo) ReverseReservationByID(context.Context, string) error    { return nil }
func (stubERPRepo) ReverseReservationsByCart(context.Context, string) error { return nil }
func (stubERPRepo) ClaimReservationForReversal(context.Context, string) (bool, error) {
	return true, nil
}
func (stubERPRepo) RestoreReservationToActive(context.Context, string) error { return nil }
func (stubERPRepo) GetCartInvoiceAnchor(context.Context, string) (string, string, error) {
	return "", "", nil
}
func (stubERPRepo) UpsertCartERPInvoice(context.Context, UpsertCartERPInvoiceParams) (int64, error) {
	return 0, nil
}
func (stubERPRepo) FindCartByExternalOrderID(context.Context, string, string) (string, error) {
	return "", nil
}
func (stubERPRepo) GetShipmentByOrderID(context.Context, string) (*ShipmentInvoiceRef, error) {
	return nil, nil
}
func (stubERPRepo) UpdateShipmentInvoice(context.Context, string, string, string) error { return nil }
func (stubERPRepo) AcquireCartFinalisationLock(context.Context, string) (func(), bool, error) {
	return func() {}, false, nil
}
func (stubERPRepo) GetCartERPOrderState(context.Context, string) (*CartERPOrderState, error) {
	return nil, nil
}
func (stubERPRepo) ListActiveReservationsByCartAndProduct(context.Context, string, string) ([]StockReservationRow, error) {
	return nil, nil
}
func (stubERPRepo) CreateStockReservation(context.Context, CreateStockReservationParams) (*StockReservationRow, error) {
	return nil, nil
}
func (stubERPRepo) UpsertActiveReservationQuantity(_ context.Context, p UpsertReservationParams) (*StockReservationRow, error) {
	return &StockReservationRow{Quantity: p.IncQty}, nil
}

func (stubERPRepo) DecrementActiveReservationQuantity(context.Context, string, string, int) (ReservationDecrement, error) {
	return ReservationDecrement{}, nil
}

func (stubERPRepo) RestoreReservationQuantityByID(context.Context, string, int) error { return nil }

func (stubERPRepo) AdjustActiveReservationQuantity(context.Context, string, string, int, string) (*StockReservationRow, error) {
	return nil, nil
}
func (stubERPRepo) ReverseReservationsByCartAndProduct(context.Context, string, string) error {
	return nil
}
func (stubERPRepo) DecrementProductStock(context.Context, string, int) error  { return nil }
func (stubERPRepo) IncrementProductStock(context.Context, string, int) error  { return nil }
func (stubERPRepo) EmitStockReserved(context.Context, StockEventParams) error { return nil }
func (stubERPRepo) EmitStockReleased(context.Context, StockEventParams) error { return nil }
func (stubERPRepo) TransitionCartERPOrderState(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (stubERPRepo) UpdateCartExternalOrderID(context.Context, string, string) error { return nil }
func (stubERPRepo) SetCartERPStockLaunched(context.Context, string, bool) error     { return nil }
func (stubERPRepo) MarkCartERPFinalisationDone(context.Context, string) error       { return nil }
func (stubERPRepo) ListNonWaitlistedCartItems(context.Context, string) ([]NonWaitlistedCartItem, error) {
	return nil, nil
}
func (stubERPRepo) ListStuckERPOrderOps(context.Context, time.Duration) ([]StuckERPOrderOp, error) {
	return nil, nil
}
func (stubERPRepo) ListTinyIntegrationsWithStaleStockWebhook(context.Context, time.Duration) ([]StaleStockWebhookIntegration, error) {
	return nil, nil
}
func (stubERPRepo) StampIntegrationStockWebhookAlert(context.Context, string) error { return nil }

// stubCollaborators is a no-op StockCollaborators for construction proofs.
type stubCollaborators struct{}

func (stubCollaborators) ResolveProvider(context.Context, *Integration) (providers.ERPProvider, error) {
	return nil, nil
}
func (stubCollaborators) NoteERPMovementSent(string) {}

func (stubCollaborators) ResolveExternalProduct(context.Context, string, string) (string, bool) {
	return "", false
}
func (stubCollaborators) OrderAtCheckoutEnabled(string) bool { return false }
func (stubCollaborators) ResolveERPContact(context.Context, providers.ERPProvider, *Integration, string, string, string, string, string, string, string) (string, error) {
	return "", nil
}
func (stubCollaborators) CreateFinalERPOrderForConversion(context.Context, providers.ERPProvider, *Integration, string, string) error {
	return nil
}
func (stubCollaborators) CreateFinalERPOrder(context.Context, providers.ERPProvider, *Integration, string, string, *providers.PaymentStatus, bool) error {
	return nil
}
func (stubCollaborators) FinalisationInverted(string) bool { return false }
func (stubCollaborators) ReReserveAfterFailedFinalisation(context.Context, providers.ERPProvider, string, []StockReservationRow) {
}
func (stubCollaborators) ReverseCartReservationsPerRow(context.Context, providers.ERPProvider, string) error {
	return nil
}
func (stubCollaborators) MarkFinalisationFailed(context.Context, string, string)                {}
func (stubCollaborators) MirrorToOrder(context.Context, string)                                 {}
func (stubCollaborators) EmitERPOrderFinalized(context.Context, string, string)                 {}
func (stubCollaborators) EmitERPOrderCancelled(context.Context, string, string, string, string) {}
func (stubCollaborators) ResolveERPProviderByID(context.Context, string, string) (providers.ERPProvider, error) {
	return nil, nil
}
func (stubCollaborators) HandleProviderError(context.Context, string, string, error) {}

// Compile-time proofs the stubs satisfy the ports.
var (
	_ ERPRepository      = stubERPRepo{}
	_ StockCollaborators = stubCollaborators{}
)

func TestNewService(t *testing.T) {
	svc := NewService(stubERPRepo{}, stubCollaborators{}, zap.NewNop())

	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.repo == nil {
		t.Error("repo not wired")
	}
	if svc.collab == nil {
		t.Error("collab not wired")
	}
	if svc.stock == nil {
		t.Error("stock not wired")
	}
	if svc.logger == nil {
		t.Error("logger not wired")
	}
}

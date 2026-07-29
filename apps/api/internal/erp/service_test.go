package erp

import (
	"context"
	"testing"

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
func (stubERPRepo) AdjustActiveReservationQuantity(context.Context, string, string, int, string) (*StockReservationRow, error) {
	return nil, nil
}
func (stubERPRepo) ReverseReservationsByCartAndProduct(context.Context, string, string) error { return nil }
func (stubERPRepo) DecrementProductStock(context.Context, string, int) error                  { return nil }
func (stubERPRepo) IncrementProductStock(context.Context, string, int) error                  { return nil }
func (stubERPRepo) EmitStockReserved(context.Context, StockEventParams) error                 { return nil }
func (stubERPRepo) EmitStockReleased(context.Context, StockEventParams) error                 { return nil }

// stubCollaborators is a no-op StockCollaborators for construction proofs.
type stubCollaborators struct{}

func (stubCollaborators) ResolveProvider(context.Context, *Integration) (providers.ERPProvider, error) {
	return nil, nil
}
func (stubCollaborators) ResolveExternalProduct(context.Context, string, string) (string, bool) {
	return "", false
}
func (stubCollaborators) MutateERPOrderItems(context.Context, string, string) error { return nil }

// Compile-time proofs the stubs satisfy the ports.
var (
	_ ERPRepository       = stubERPRepo{}
	_ StockCollaborators  = stubCollaborators{}
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

package erp

import (
	"context"

	"go.uber.org/zap"
)

// ERPRepository is the persistence port consumed by the ERP service. It is
// declared here (the consumer side, per Go idiom) and satisfied by
// internal/integration.Repository via a boot-wired adapter — erp MUST NOT import
// integration (cycle). It starts with the two methods the ERP order flow already
// needs and grows one method per slice as logic migrates (Bloco B2b+).
type ERPRepository interface {
	// GetActiveByProvider resolves the active ERP integration for a store.
	// Signature mirrors integration.Repository.GetActiveByProvider, but returns
	// the neutral erp.Integration instead of the integration-owned IntegrationRow
	// so this package stays free of the integration import.
	GetActiveByProvider(ctx context.Context, storeID, integrationType, provider string) (*Integration, error)

	// AcquireCartFinalisationLock takes a session-scoped Postgres advisory lock
	// keyed on the cart id. The lock lives on integration.Repository (which holds
	// the raw pgxpool — advisory locks are per-connection); erp only consumes it
	// through this port. acquired=false means another finalisation of the SAME
	// cart is running right now, so the loser bails. The caller MUST call
	// release() when acquired.
	AcquireCartFinalisationLock(ctx context.Context, cartID string) (release func(), acquired bool, err error)

	// --- Stock reservation persistence (Bloco B2b) ---
	// Every method below is already implemented verbatim on integration.Repository;
	// it satisfies this port directly (the DTO types are aliases of the erp ones),
	// so only GetActiveByProvider needs an adapter for its return type.

	// GetCartERPOrderState reads the cart's order-as-reservation lifecycle state.
	GetCartERPOrderState(ctx context.Context, cartID string) (*CartERPOrderState, error)
	// ListActiveReservationsByCartAndProduct returns the active reservations for a
	// cart+product (in practice the unique index keeps it to one row).
	ListActiveReservationsByCartAndProduct(ctx context.Context, cartID, productID string) ([]StockReservationRow, error)
	// CreateStockReservation persists a new stock reservation row.
	CreateStockReservation(ctx context.Context, params CreateStockReservationParams) (*StockReservationRow, error)
	// AdjustActiveReservationQuantity bumps an active reservation's quantity by delta.
	AdjustActiveReservationQuantity(ctx context.Context, cartID, productID string, delta int, erpMovementID string) (*StockReservationRow, error)
	// ReverseReservationsByCartAndProduct marks a cart+product's reservations reversed.
	ReverseReservationsByCartAndProduct(ctx context.Context, cartID, productID string) error
	// DecrementProductStock atomically lowers local stock; ErrNoRows means the
	// decrement would go negative (insufficient stock).
	DecrementProductStock(ctx context.Context, productID string, quantity int) error
	// IncrementProductStock raises local stock (also satisfies stockReservationRepo).
	IncrementProductStock(ctx context.Context, productID string, quantity int) error
	// EmitStockReserved / EmitStockReleased publish the stock events best-effort
	// (also satisfy stockReservationRepo, so NewStockReservations can take repo).
	EmitStockReserved(ctx context.Context, p StockEventParams) error
	EmitStockReleased(ctx context.Context, p StockEventParams) error
}

// Service handles ERP-domain business logic. B2a laid the foundation (struct +
// ports); B2b moves in the cart→ERP stock reservation flow (ReserveStockInERP /
// AdjustStockReservationDelta). More order/finalisation logic follows in B2c+.
type Service struct {
	repo   ERPRepository
	collab StockCollaborators
	stock  *StockReservations
	logger *zap.Logger
}

// NewService creates a new ERP service. collab supplies the integration-Service
// helpers the migrated stock flow still calls back into (provider resolution,
// product linking, converted-cart mutation); it shrinks as more logic migrates.
func NewService(repo ERPRepository, collab StockCollaborators, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		collab: collab,
		stock:  NewStockReservations(repo, logger),
		logger: logger,
	}
}

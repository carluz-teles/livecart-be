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
}

// Service handles ERP-domain business logic. B2a is the foundation: the struct,
// constructor and ports exist and compile; the actual order/finalisation/stock
// logic is moved in from internal/integration in later slices (B2b+).
type Service struct {
	repo   ERPRepository
	logger *zap.Logger
}

// NewService creates a new ERP service.
func NewService(repo ERPRepository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger,
	}
}

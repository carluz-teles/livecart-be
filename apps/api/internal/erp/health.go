package erp

import (
	"context"
	"fmt"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// =============================================================================
// ERP HEALTH CHECK — moved from internal/integration (Bloco B2d). integration
// keeps a one-liner delegation; the HTTP handler call site migrates in B2e.
// =============================================================================

// ERPHealthCheckResponse mirrors providers.ERPHealthCheckResult shaped for
// the JSON envelope. Items are flattened so the FE can group by category
// without re-traversing. Canonical home is this package (Bloco B2d);
// internal/integration aliases it so the handler and the delegation keep
// compiling unchanged.
type ERPHealthCheckResponse struct {
	Supported bool                           `json:"supported"`
	CheckedAt time.Time                      `json:"checkedAt"`
	Items     []providers.ERPHealthCheckItem `json:"items"`
}

// RunERPHealthCheck audits the merchant's cadastros against the canonical
// names the order-creation flow looks up (formas-pagamento,
// formas-recebimento, formas-envio). Returns supported=false when the
// underlying ERP provider doesn't implement the optional ERPHealthChecker
// capability, so the FE can hide the section instead of erroring.
func (s *Service) RunERPHealthCheck(ctx context.Context, integrationID, storeID string) (*ERPHealthCheckResponse, error) {
	erpProvider, err := s.collab.ResolveERPProviderByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	checker, ok := erpProvider.(providers.ERPHealthChecker)
	if !ok {
		return &ERPHealthCheckResponse{
			Supported: false,
			CheckedAt: time.Now(),
			Items:     nil,
		}, nil
	}

	result, err := checker.HealthCheck(ctx)
	if err != nil {
		s.collab.HandleProviderError(ctx, integrationID, "erp_health_check", err)
		return nil, fmt.Errorf("running ERP health check: %w", err)
	}

	return &ERPHealthCheckResponse{
		Supported: true,
		CheckedAt: result.CheckedAt,
		Items:     result.Items,
	}, nil
}

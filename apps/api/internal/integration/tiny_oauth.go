package integration

import (
	"context"
	"livecart/apps/api/lib/httpx"
)

// OAuth must still find the app after a failed refresh marked it as errored.
// Only the configured Tiny integration can be reauthorized; another ERP must
// never inherit these credentials.
func (s *Service) tinyOAuthIntegration(ctx context.Context, storeID string) (*IntegrationRow, error) {
	row, err := s.repo.GetByProvider(ctx, storeID, "erp", "tiny")
	if err == nil {
		return row, nil
	}
	if !httpx.IsNotFound(err) {
		return nil, err
	}
	row, err = s.repo.GetAnyByType(ctx, storeID, "erp")
	if err != nil {
		return nil, err
	}
	if row == nil || row.Provider != "tiny" || row.Status != "error" {
		return nil, httpx.ErrNotFound("Integração Tiny não encontrada")
	}
	return row, nil
}

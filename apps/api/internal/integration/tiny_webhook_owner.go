package integration

import (
	"context"
	"fmt"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
)

var errTinyWebhookConnectionPending = events.NewDeferredError("Tiny webhook awaits integration recovery")

// An OAuth failure pauses consumption, not durable receipt. An intentional
// disconnection or a different ERP must never be revived by a webhook.
func (s *Service) tinyWebhookOwner(ctx context.Context, storeID string) (*IntegrationRow, error) {
	row, err := s.repo.GetAnyByType(ctx, storeID, "erp")
	if httpx.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolving Tiny webhook owner: %w", err)
	}
	if row.Provider != "tiny" || (row.Status != "active" && row.Status != "error") {
		return nil, nil
	}
	return row, nil
}

func (s *Service) tinyWebhookConsumerOwner(ctx context.Context, storeID, integrationID string) (*IntegrationRow, error) {
	row, err := s.tinyWebhookOwner(ctx, storeID)
	if err != nil || row == nil {
		return nil, err
	}
	if row.ID != integrationID {
		return nil, nil
	}
	if row.Status != "active" {
		return nil, errTinyWebhookConnectionPending
	}
	return row, nil
}

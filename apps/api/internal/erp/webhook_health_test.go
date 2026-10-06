package erp

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type webhookHealthRepo struct {
	stubERPRepo
	listErr error
	stamped []string
}

func (r *webhookHealthRepo) ListTinyIntegrationsWithStaleStockWebhook(context.Context, time.Duration) ([]StaleStockWebhookIntegration, error) {
	return []StaleStockWebhookIntegration{{IntegrationID: "quiet", StoreID: "store"}}, r.listErr
}

func (r *webhookHealthRepo) StampIntegrationStockWebhookAlert(_ context.Context, id string) error {
	r.stamped = append(r.stamped, id)
	return nil
}

func TestStockWebhookSilenceIsWarningButInspectionFailureIsError(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	repo := &webhookHealthRepo{}
	svc := NewService(repo, stubCollaborators{}, zap.New(core))
	svc.CheckTinyStockWebhookDelivery(t.Context(), 12*time.Hour)
	if logs.FilterLevelExact(zap.WarnLevel).Len() != 1 || logs.FilterLevelExact(zap.ErrorLevel).Len() != 0 || len(repo.stamped) != 1 {
		t.Fatalf("quiet store should emit one deduplicated warning: %+v", logs.All())
	}
	repo.listErr = errors.New("database unavailable")
	svc.CheckTinyStockWebhookDelivery(t.Context(), 12*time.Hour)
	if logs.FilterLevelExact(zap.ErrorLevel).Len() != 1 || len(repo.stamped) != 1 {
		t.Fatal("actual health check failure was hidden or falsely stamped")
	}
}

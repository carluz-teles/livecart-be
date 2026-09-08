package integration

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

func TestTokenWorkerDoesNotCountMissingRefreshCredentialAsSuccess(t *testing.T) {
	svc := reconnectTestService(t)
	encrypted, err := svc.encryptor.EncryptJSON(&providers.Credentials{AccessToken: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewTokenRefreshWorker(TokenRefreshWorkerConfig{Service: svc, Logger: zap.NewNop()})
	updated, err := worker.refreshToken(context.Background(), &IntegrationRow{
		Provider: "bling", Credentials: encrypted,
	})
	if err != nil || updated {
		t.Fatalf("skipped token counted as refreshed: updated=%v err=%v", updated, err)
	}
}

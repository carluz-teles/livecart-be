//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

type forbiddenRefreshError struct{}

func (*forbiddenRefreshError) Error() string   { return "bling: token endpoint HTTP 403 (oauth_error)" }
func (*forbiddenRefreshError) Status() int     { return 403 }
func (*forbiddenRefreshError) Permanent() bool { return false }

func TestBling403CooldownSurvivesStaleReadsAndReconnectionClearsIt(t *testing.T) {
	svc, row, old := seedBlingRefresh(t)
	calls := 0
	failure := &forbiddenRefreshError{}
	svc.factory = providers.NewFactory(providers.FactoryConfig{
		Logger: zap.NewNop(),
		BlingConstructor: func(providers.BlingConfig) (providers.ERPProvider, error) {
			return refreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) { calls++; return nil, failure }}, nil
		},
	})
	if _, err := svc.refreshToken(t.Context(), row, old); !errors.Is(err, failure) {
		t.Fatalf("lost provider error: %v", err)
	}
	fresh, err := testRepo.GetByID(t.Context(), row.ID, row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "active" || !tokenRefreshDeferred(fresh, time.Now()) || tokenRefreshDeferred(fresh, time.Now().Add(61*time.Minute)) {
		t.Fatalf("invalid cooldown: status=%s metadata=%v", fresh.Status, fresh.Metadata)
	}
	if _, err := svc.refreshToken(t.Context(), row, old); !errors.Is(err, errTokenRefreshDeferred) {
		t.Fatalf("stale caller ignored cooldown: %v", err)
	}
	if calls != 1 {
		t.Fatalf("provider was called %d times", calls)
	}
	rows, err := testRepo.ListWithExpiringTokens(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range rows {
		if candidate.ID == row.ID {
			t.Fatal("worker selected paused connection")
		}
	}
	// Both successful rotation and OAuth reconnection use UpdateCredentials.
	if err := testRepo.UpdateCredentials(t.Context(), row.ID, row.Credentials, &old.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	fresh, err = testRepo.GetByID(t.Context(), row.ID, row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	if tokenRefreshDeferred(fresh, time.Now()) || fresh.Metadata["tokenRefreshFailure"] != nil {
		t.Fatal("reconnection retained failure state")
	}
}

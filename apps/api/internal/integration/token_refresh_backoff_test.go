//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

type authRefreshError struct {
	status    int
	permanent bool
}

func (e *authRefreshError) Error() string {
	return fmt.Sprintf("bling: token endpoint HTTP %d", e.status)
}
func (e *authRefreshError) Status() int     { return e.status }
func (e *authRefreshError) Permanent() bool { return e.permanent }

func TestBlingAuthCooldownSurvivesStaleReadsAndReconnectionClearsIt(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			testBlingAuthCooldown(t, status)
		})
	}
}

func testBlingAuthCooldown(t *testing.T, status int) {
	t.Helper()
	svc, row, old := seedBlingRefresh(t)
	calls := 0
	failure := &authRefreshError{status: status}
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
	failureState := fresh.Metadata["tokenRefreshFailure"].(map[string]any)
	if failureState["statusCode"] != float64(status) {
		t.Fatalf("wrong diagnostic status: %v", failureState)
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
	// Expiring the durable pause must allow one new attempt, even for a stale caller.
	failureState["nextAttemptAt"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := testRepo.UpdateMetadata(t.Context(), row.ID, fresh.Metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.refreshToken(t.Context(), row, old); !errors.Is(err, failure) || calls != 2 {
		t.Fatalf("expired cooldown did not allow a new attempt: calls=%d err=%v", calls, err)
	}
	fresh, err = testRepo.GetByID(t.Context(), row.ID, row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Metadata["tokenRefreshFailure"].(map[string]any)["attempts"] != float64(2) {
		t.Fatal("cooldown attempt count was not persisted")
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

func TestBlingPermanentAuthFailureRequiresReconnectionWithoutCooldown(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			svc, row, old := seedBlingRefresh(t)
			failure := &authRefreshError{status: status, permanent: true}
			svc.factory = providers.NewFactory(providers.FactoryConfig{
				Logger: zap.NewNop(),
				BlingConstructor: func(providers.BlingConfig) (providers.ERPProvider, error) {
					return refreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) {
						return nil, failure
					}}, nil
				},
			})
			if _, err := svc.refreshToken(t.Context(), row, old); !errors.Is(err, failure) {
				t.Fatalf("lost permanent cause: %v", err)
			}
			fresh, err := testRepo.GetByID(t.Context(), row.ID, row.StoreID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Status != "error" || fresh.Metadata["tokenRefreshFailure"] != nil {
				t.Fatalf("revoked credentials treated as temporary: %s %v", fresh.Status, fresh.Metadata)
			}
		})
	}
}

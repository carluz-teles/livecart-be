//go:build integration

package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

func seedTinyRefresh(t *testing.T) (*Service, *IntegrationRow, *providers.Credentials) {
	t.Helper()
	svc, row, creds := seedBlingRefresh(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET provider='tiny' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	row.Provider = "tiny"
	return svc, row, creds
}

func TestTinyRefreshRetriesTransientErrorsAndKeepsPermanentFailuresPaused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   error
		permanent bool
	}{
		{"gateway timeout", &authRefreshError{status: 504}, false},
		{"network timeout", context.DeadlineExceeded, false},
		{"network canceled", context.Canceled, false},
		{"rate limit", &authRefreshError{status: 429}, false},
		{"revoked", &authRefreshError{status: 400, permanent: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, row, old := seedTinyRefresh(t)
			calls := 0
			failure := tc.failure
			svc.factory = providers.NewFactory(providers.FactoryConfig{
				Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
					return refreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) {
						calls++
						if failure != nil {
							return nil, failure
						}
						return &providers.Credentials{AccessToken: "new", RefreshToken: "rotated", ExpiresAt: time.Now().Add(4 * time.Hour)}, nil
					}}, nil
				},
			})
			if _, err := svc.refreshToken(t.Context(), row, old); !errors.Is(err, tc.failure) {
				t.Fatalf("lost cause: %v", err)
			}
			current, err := testRepo.GetByID(t.Context(), row.ID, row.StoreID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.permanent {
				if current.Status != "error" || tokenRefreshDeferred(current, time.Now()) {
					t.Fatal("revoked credential was kept active")
				}
				if _, err := svc.refreshToken(t.Context(), row, old); err == nil || calls != 1 {
					t.Fatal("stale caller renewed disabled connection")
				}
				return
			}
			if current.Status != "active" || !tokenRefreshDeferred(current, time.Now()) {
				t.Fatal("transient failure disconnected the store")
			}
			if _, err := svc.refreshToken(t.Context(), row, old); !errors.Is(err, errTokenRefreshDeferred) || calls != 1 {
				t.Fatal("stale caller bypassed cooldown")
			}
			rows, err := testRepo.ListWithExpiringTokens(t.Context(), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range rows {
				if candidate.ID == row.ID {
					t.Fatal("worker selected paused token")
				}
			}
			state := current.Metadata["tokenRefreshFailure"].(map[string]any)
			state["nextAttemptAt"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			if err := testRepo.UpdateMetadata(t.Context(), row.ID, current.Metadata); err != nil {
				t.Fatal(err)
			}
			failure = nil
			if _, err := svc.refreshToken(t.Context(), row, old); err != nil {
				t.Fatal(err)
			}
			current, err = testRepo.GetByID(t.Context(), row.ID, row.StoreID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "active" || current.Metadata["tokenRefreshFailure"] != nil {
				t.Fatal("successful recovery did not clear cooldown")
			}
		})
	}
}

func TestTinyRefreshSerializesRotationAcrossServiceInstances(t *testing.T) {
	svc, row, old := seedTinyRefresh(t)
	var calls atomic.Int32
	entered, proceed := make(chan struct{}), make(chan struct{})
	factory := providers.NewFactory(providers.FactoryConfig{
		Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
			return refreshTestProvider{refresh: func(ctx context.Context) (*providers.Credentials, error) {
				if calls.Add(1) != 1 {
					return nil, errors.New("refresh token reused")
				}
				close(entered)
				select {
				case <-proceed:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &providers.Credentials{AccessToken: "new", RefreshToken: "rotated", ExpiresAt: time.Now().Add(4 * time.Hour)}, nil
			}}, nil
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan error, 8)
	for range 8 {
		other := &Service{repo: NewRepository(testRepo.queries, testPool), factory: factory, encryptor: svc.encryptor, logger: zap.NewNop()}
		go func() {
			creds, err := other.refreshToken(ctx, row, old)
			if err == nil && creds.RefreshToken != "rotated" {
				err = errors.New("lost rotated token")
			}
			results <- err
		}()
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(proceed)
	for range 8 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("OAuth called %d times", calls.Load())
	}
}

func TestTinyRefreshDoesNotReactivateDisconnectedIntegration(t *testing.T) {
	svc, row, old := seedTinyRefresh(t)
	if err := testRepo.UpdateStatus(t.Context(), row.ID, "disconnected"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.refreshToken(t.Context(), row, old); err == nil {
		t.Fatal("stale refresh revived disconnected integration")
	}
	current, err := testRepo.GetByID(t.Context(), row.ID, row.StoreID)
	if err != nil || current.Status != "disconnected" {
		t.Fatalf("disconnected state changed: %v", err)
	}
}

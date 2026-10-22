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

func TestTinyRefreshPreservesRotationAfterCallerCancellation(t *testing.T) {
	for _, cancelBeforeResponse := range []bool{true, false} {
		name := "after provider response"
		if cancelBeforeResponse {
			name = "while provider responds"
		}
		t.Run(name, func(t *testing.T) {
			svc, row, old := seedTinyRefresh(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			svc.factory = providers.NewFactory(providers.FactoryConfig{
				Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
					return refreshTestProvider{refresh: func(refreshCtx context.Context) (*providers.Credentials, error) {
						calls++
						// The provider has consumed the old refresh token. A browser
						// disconnect must not discard its response or the DB commit.
						cancel()
						if cancelBeforeResponse && refreshCtx.Err() != nil {
							return nil, refreshCtx.Err()
						}
						if _, bounded := refreshCtx.Deadline(); !bounded {
							t.Error("OAuth exchange has no deadline")
						}
						return &providers.Credentials{
							AccessToken: "new-access", RefreshToken: "rotated-refresh",
							ExpiresAt: time.Now().Add(4 * time.Hour),
						}, nil
					}}, nil
				},
			})
			if _, err := svc.refreshToken(ctx, row, old); err != nil {
				t.Fatalf("caller cancellation lost rotation: %v", err)
			}
			current, err := testRepo.GetByID(t.Context(), row.ID, row.StoreID)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := svc.decryptCredentials(current.Credentials)
			if err != nil {
				t.Fatal(err)
			}
			if saved.RefreshToken != "rotated-refresh" || current.Status != "active" {
				t.Fatal("rotated credentials were not committed")
			}
			// A worker holding the old snapshot must reuse the committed result.
			if _, err := svc.refreshToken(t.Context(), row, old); err != nil || calls != 1 {
				t.Fatalf("old refresh token reused: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestTinyRefreshCancelledBeforeLockDoesNotCallProvider(t *testing.T) {
	svc, row, old := seedTinyRefresh(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := svc.refreshToken(ctx, row, old); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller started refresh: %v", err)
	}
}

func TestTinyRefreshLockWaitRespectsCallerDeadline(t *testing.T) {
	svc, row, old := seedTinyRefresh(t)
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(t.Context(), "SELECT id FROM integrations WHERE id=$1 FOR NO KEY UPDATE", row.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, refreshErr := svc.refreshToken(ctx, row, old)
		result <- refreshErr
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting caller did not preserve deadline: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled waiter is still waiting for the row lock")
	}
}

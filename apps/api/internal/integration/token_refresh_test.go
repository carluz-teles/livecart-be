//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

type refreshTestProvider struct {
	providers.ERPProvider
	refresh func(context.Context) (*providers.Credentials, error)
}

func (p refreshTestProvider) RefreshToken(ctx context.Context) (*providers.Credentials, error) {
	return p.refresh(ctx)
}

type refreshTestError struct{ permanent bool }

func (e *refreshTestError) Error() string   { return "oauth refresh rejected" }
func (e *refreshTestError) Permanent() bool { return e.permanent }

func seedBlingRefresh(t *testing.T) (*Service, *IntegrationRow, *providers.Credentials) {
	t.Helper()
	requireDB(t)
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)
	creds := &providers.Credentials{
		AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Minute).UTC().Truncate(time.Second),
	}
	encrypted, err := svc.encryptor.EncryptJSON(creds)
	if err != nil {
		t.Fatal(err)
	}
	row, err := testRepo.Create(context.Background(), CreateIntegrationParams{
		StoreID: storeID, Type: "erp", Provider: "bling", Status: "active",
		Credentials: encrypted, TokenExpiresAt: &creds.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, row, creds
}

func TestBlingRefreshErrorsPreserveCauseAndConnection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{name: "rate limit", err: &refreshTestError{}, status: "active"},
		{name: "network failure", err: errors.New("temporary network failure"), status: "active"},
		{name: "timeout", err: context.DeadlineExceeded, status: "active"},
		{name: "cancellation", err: context.Canceled, status: "active"},
		{name: "revoked credentials", err: &refreshTestError{permanent: true}, status: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, row, _ := seedBlingRefresh(t)
			calls := 0
			failure := tc.err
			svc.factory = providers.NewFactory(providers.FactoryConfig{
				Logger: zap.NewNop(),
				BlingConstructor: func(cfg providers.BlingConfig) (providers.ERPProvider, error) {
					calls++
					if cfg.Credentials == nil {
						t.Error("provider received nil credentials after refresh failure")
					}
					return refreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) {
						if failure != nil {
							return nil, fmt.Errorf("token endpoint: %w", failure)
						}
						return &providers.Credentials{
							AccessToken: "recovered-access", RefreshToken: "recovered-refresh",
							ExpiresAt: time.Now().Add(6 * time.Hour),
						}, nil
					}}, nil
				},
			})
			_, err := svc.createProviderFromRow(context.Background(), row)
			if !errors.Is(err, tc.err) {
				t.Fatalf("original error lost: %v", err)
			}
			if calls != 1 {
				t.Fatalf("provider constructed %d times; failure must stop the caller", calls)
			}
			current, err := testRepo.GetByID(context.Background(), row.ID, row.StoreID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != tc.status {
				t.Fatalf("status = %s, expected %s", current.Status, tc.status)
			}
			if tc.status == "active" {
				failure = nil
				if _, err := svc.createProviderFromRow(context.Background(), current); err != nil {
					t.Fatalf("connection could not retry after temporary failure: %v", err)
				}
			}
		})
	}
}

func TestBlingRefreshSerializesIndependentServiceInstances(t *testing.T) {
	svc, row, old := seedBlingRefresh(t)
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	newCreds := &providers.Credentials{
		AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(6 * time.Hour),
	}
	factory := providers.NewFactory(providers.FactoryConfig{
		Logger: zap.NewNop(),
		BlingConstructor: func(cfg providers.BlingConfig) (providers.ERPProvider, error) {
			return refreshTestProvider{refresh: func(ctx context.Context) (*providers.Credentials, error) {
				if calls.Add(1) != 1 {
					return nil, errors.New("refresh token was already consumed")
				}
				entered <- struct{}{}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-proceed:
					return newCreds, nil
				}
			}}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const concurrent = 8
	results := make(chan error, concurrent)
	var started sync.WaitGroup
	started.Add(concurrent)
	for range concurrent {
		// Separate repositories and services: an in-memory lock on a Service
		// would not protect these callers sharing the database.
		other := &Service{
			repo: NewRepository(testRepo.queries, testPool), factory: factory,
			encryptor: svc.encryptor, logger: zap.NewNop(),
		}
		go func() {
			started.Done()
			creds, err := other.refreshToken(ctx, row, old)
			if err == nil && (creds == nil || creds.RefreshToken != "new-refresh") {
				err = errors.New("caller did not receive the persisted rotated token")
			}
			results <- err
		}()
	}
	started.Wait()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(proceed)
	for range concurrent {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, expected 1", n)
	}
	current, err := testRepo.GetByID(ctx, row.ID, row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := svc.decryptCredentials(current.Credentials)
	if err != nil || saved.RefreshToken != "new-refresh" || current.Status != "active" {
		t.Fatalf("rotated token not saved: status=%s err=%v", current.Status, err)
	}
}

func TestBlingRefreshDoesNotReportSuccessWhenPersistenceFails(t *testing.T) {
	svc, row, old := seedBlingRefresh(t)
	ctx := context.Background()
	// Constraint applies only to this fixture. The new rotated access token
	// changes the ciphertext, making the credentials UPDATE fail after OAuth.
	constraint := "reject_refresh_" + strings.ReplaceAll(row.ID, "-", "")
	ddl := fmt.Sprintf(`ALTER TABLE integrations ADD CONSTRAINT %s CHECK (
		id <> '%s'::uuid OR credentials = decode('%x', 'hex'))`, constraint, row.ID, row.Credentials)
	if _, err := testPool.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "ALTER TABLE integrations DROP CONSTRAINT "+constraint); err != nil {
			t.Error(err)
		}
	})
	svc.factory = providers.NewFactory(providers.FactoryConfig{
		Logger: zap.NewNop(),
		BlingConstructor: func(providers.BlingConfig) (providers.ERPProvider, error) {
			return refreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) {
				return &providers.Credentials{
					AccessToken: "rotated-access", RefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(time.Hour),
				}, nil
			}}, nil
		},
	})
	creds, err := svc.refreshToken(ctx, row, old)
	if err == nil || creds != nil || !strings.Contains(err.Error(), "saving new credentials") {
		t.Fatalf("persistence failure reported as success: creds_present=%v err=%v", creds != nil, err)
	}
}

func TestBlingRefreshReusesRotatedTokenEvenWhenTokenEndpointIsBlocked(t *testing.T) {
	svc, row, previous := seedBlingRefresh(t)
	current := &providers.Credentials{AccessToken: "already-rotated", RefreshToken: "already-rotated-refresh", ExpiresAt: time.Now().Add(6 * time.Hour)}
	encrypted, err := svc.encryptor.EncryptJSON(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := testRepo.UpdateCredentials(context.Background(), row.ID, encrypted, &current.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	manager := ratelimit.NewManager(zap.NewNop())
	if err := manager.GetOrCreateBling("bling:oauth:shared-egress", 0.25).BlockFor(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	svc.factory = providers.NewFactory(providers.FactoryConfig{RateLimitManager: manager})
	got, err := svc.refreshToken(context.Background(), row, previous)
	if err != nil || got == nil || got.AccessToken != current.AccessToken {
		t.Fatalf("valid saved token rejected by token quota: credentials=%v err=%v", got != nil, err)
	}
}

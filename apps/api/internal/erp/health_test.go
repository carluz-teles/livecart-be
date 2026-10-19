package erp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// fakeHealthProvider satisfies providers.ERPHealthChecker (and, via the embedded
// interface, providers.ERPProvider). It returns a configured result/err.
type fakeHealthProvider struct {
	providers.ERPProvider
	result *providers.ERPHealthCheckResult
	err    error
	checks int
	check  func(context.Context) (*providers.ERPHealthCheckResult, error)
}

func (f *fakeHealthProvider) HealthCheck(ctx context.Context) (*providers.ERPHealthCheckResult, error) {
	f.checks++
	if f.check != nil {
		return f.check(ctx)
	}
	return f.result, f.err
}

func TestHealthCheckHasBoundedLifetime(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parentTimeout time.Duration
		providerDelay time.Duration
		wantElapsed   time.Duration
		wantErr       error
	}{
		{"slow success", time.Minute, 21 * time.Second, 21 * time.Second, nil},
		{"audit deadline", time.Minute, time.Minute, 30 * time.Second, context.DeadlineExceeded},
		{"shorter parent deadline", 3 * time.Second, time.Minute, 3 * time.Second, context.DeadlineExceeded},
		{"already cancelled", 0, time.Minute, 0, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), tc.parentTimeout)
				defer cancel()
				var providerCtx context.Context
				fake := &fakeHealthProvider{check: func(ctx context.Context) (*providers.ERPHealthCheckResult, error) {
					providerCtx = ctx
					select {
					case <-time.After(tc.providerDelay):
						return &providers.ERPHealthCheckResult{CheckedAt: time.Now()}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}}
				collab := &mockCollab{providerByID: fake}
				start := time.Now()
				resp, err := newSvc(&mockRepo{}, collab).RunERPHealthCheck(ctx, "integration", "store")
				if !errors.Is(err, tc.wantErr) || time.Since(start) != tc.wantElapsed {
					t.Fatalf("result=%+v err=%v elapsed=%s", resp, err, time.Since(start))
				}
				if tc.wantErr == nil && (resp == nil || !resp.Supported) {
					t.Fatal("a slow successful audit must remain supported")
				}
				if providerCtx.Err() == nil {
					t.Fatal("provider context must be released after the audit returns")
				}
			})
		})
	}
}

func TestService_RunERPHealthCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("provider without the capability reports unsupported", func(t *testing.T) {
		// fakeERPProvider (from stock_service_test.go) is an ERPProvider that does
		// NOT implement ERPHealthChecker — exactly the Bling/Omie case.
		repo := &mockRepo{}
		collab := &mockCollab{providerByID: &fakeERPProvider{}}
		resp, err := newSvc(repo, collab).RunERPHealthCheck(ctx, "int-1", "s")
		if err != nil {
			t.Fatalf("unsupported must not error, got %v", err)
		}
		if resp == nil || resp.Supported {
			t.Fatalf("expected supported=false, got %+v", resp)
		}
	})

	t.Run("supported provider surfaces the check result", func(t *testing.T) {
		checkedAt := time.Now()
		fake := &fakeHealthProvider{result: &providers.ERPHealthCheckResult{
			CheckedAt: checkedAt,
			Items:     []providers.ERPHealthCheckItem{{Category: "formas-pagamento"}},
		}}
		repo := &mockRepo{}
		collab := &mockCollab{providerByID: fake}
		resp, err := newSvc(repo, collab).RunERPHealthCheck(ctx, "int-1", "s")
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if resp == nil || !resp.Supported {
			t.Fatalf("expected supported=true, got %+v", resp)
		}
		if len(resp.Items) != 1 || !resp.CheckedAt.Equal(checkedAt) {
			t.Fatalf("result not carried through: %+v", resp)
		}
	})

	t.Run("provider resolution error propagates", func(t *testing.T) {
		repo := &mockRepo{}
		collab := &mockCollab{providerByIDErr: errors.New("no such integration")}
		if _, err := newSvc(repo, collab).RunERPHealthCheck(ctx, "int-x", "s"); err == nil {
			t.Fatal("expected the resolution error to propagate")
		}
	})

	t.Run("check error flags telemetry and propagates", func(t *testing.T) {
		fake := &fakeHealthProvider{err: errors.New("tiny down")}
		repo := &mockRepo{}
		collab := &mockCollab{providerByID: fake}
		if _, err := newSvc(repo, collab).RunERPHealthCheck(ctx, "int-1", "s"); err == nil {
			t.Fatal("expected the check error to propagate")
		}
		if collab.handledErrors != 1 {
			t.Fatalf("check failure must flag provider error once, got %d", collab.handledErrors)
		}
	})
}

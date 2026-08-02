package erp

import (
	"context"
	"errors"
	"testing"
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
}

func (f *fakeHealthProvider) HealthCheck(context.Context) (*providers.ERPHealthCheckResult, error) {
	f.checks++
	return f.result, f.err
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

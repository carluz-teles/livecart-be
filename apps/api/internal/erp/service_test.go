package erp

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// mockERPRepo is an empty stub proving ERPRepository is implementable without a
// real repository — the point of declaring the port on the consumer side. As the
// interface grows in later slices, this stub must grow with it or the build breaks.
type mockERPRepo struct{}

func (mockERPRepo) GetActiveByProvider(context.Context, string, string, string) (*Integration, error) {
	return nil, nil
}

func (mockERPRepo) AcquireCartFinalisationLock(context.Context, string) (func(), bool, error) {
	return func() {}, false, nil
}

// Compile-time proof the empty stub satisfies the port.
var _ ERPRepository = mockERPRepo{}

func TestNewService(t *testing.T) {
	svc := NewService(mockERPRepo{}, zap.NewNop())

	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.repo == nil {
		t.Error("repo not wired")
	}
	if svc.logger == nil {
		t.Error("logger not wired")
	}
}

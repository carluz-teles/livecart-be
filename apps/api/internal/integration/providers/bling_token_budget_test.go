package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"livecart/apps/api/lib/ratelimit"
)

type blingTokenQuotaError struct{}

func (blingTokenQuotaError) Error() string { return "token quota" }
func (blingTokenQuotaError) Status() int   { return 429 }

func TestBlingTokenQuotaBlocksEveryTokenCallerButNotOrderAccount(t *testing.T) {
	factory, manager := factoryComBling(t)
	ctx := context.Background()
	if err := factory.WaitBlingToken(ctx); err != nil {
		t.Fatal(err)
	}
	if err := factory.ObserveBlingTokenError(ctx, errors.Join(errors.New("refresh failed"), blingTokenQuotaError{})); err != nil {
		t.Fatal(err)
	}
	another := NewFactory(FactoryConfig{RateLimitManager: manager})
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := another.WaitBlingToken(short); !errors.Is(err, ratelimit.ErrNaoDespachado) {
		t.Fatalf("shared token block lost: %v", err)
	}
	if err := manager.GetOrCreateBling("merchant-orders", 2).Wait(ctx); err != nil {
		t.Fatalf("token quota blocked ordinary API account: %v", err)
	}
}

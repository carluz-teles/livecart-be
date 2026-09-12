package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"livecart/apps/api/lib/ratelimit"
)

func TestRetryERPProductSync(t *testing.T) {
	t.Parallel()
	permanent := errors.New("product removed from ERP")
	for _, tt := range []struct {
		name      string
		outcomes  []stockMirrorOutcome
		errors    []error
		wantCalls int
		wantErr   error
		minWait   time.Duration
	}{
		{name: "applied", outcomes: []stockMirrorOutcome{stockMirrorApplied}, wantCalls: 1},
		{name: "fresh read after stale stock", outcomes: []stockMirrorOutcome{stockMirrorStale, stockMirrorApplied}, wantCalls: 2},
		{name: "persistent stale is failure", outcomes: []stockMirrorOutcome{stockMirrorStale}, wantCalls: 5, wantErr: errResyncStockDeferred},
		{name: "rate limit retries", errors: []error{&ratelimit.ErrRateLimited{RetryAfter: time.Second}, nil}, wantCalls: 2},
		{name: "provider retry-after is respected", errors: []error{&ratelimit.ErrRateLimited{RetryAfter: 30 * time.Second}, nil}, wantCalls: 2, minWait: 30 * time.Second},
		{name: "permanent errors stop", errors: []error{permanent}, wantCalls: 1, wantErr: permanent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				started := time.Now()
				err := retryERPProductSync(t.Context(), func(context.Context) (stockMirrorOutcome, error) {
					i := calls
					calls++
					outcome := stockMirrorApplied
					if len(tt.outcomes) > 0 {
						outcome = tt.outcomes[min(i, len(tt.outcomes)-1)]
					}
					var err error
					if len(tt.errors) > 0 {
						err = tt.errors[min(i, len(tt.errors)-1)]
					}
					return outcome, err
				})
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got error %v, want %v", err, tt.wantErr)
				}
				if got := calls; got != tt.wantCalls {
					t.Fatalf("got %v, want %v", got, tt.wantCalls)
				}
				if time.Since(started) < tt.minWait {
					t.Fatalf("retried before provider retry-after: waited %s, want at least %s", time.Since(started), tt.minWait)
				}
			})
		})
	}
}

func TestRetryERPProductSyncMissingTargetAndCancellation(t *testing.T) {
	t.Parallel()
	err := retryERPProductSync(t.Context(), func(context.Context) (stockMirrorOutcome, error) {
		return stockMirrorNoTarget, nil
	})
	if err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("got error %v, want message containing %q", err, "no longer available")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	err = retryERPProductSync(ctx, func(context.Context) (stockMirrorOutcome, error) {
		calls++
		cancel()
		return stockMirrorStale, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want %v", err, context.Canceled)
	}
	if got := calls; got != 1 {
		t.Fatalf("got %v, want %v", got, 1)
	}
}

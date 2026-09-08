//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/lib/ratelimit"
)

func TestRateBudget_ReplicasShareOneDispatchSchedule(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	key := "test:" + fx.storeID
	a, b := ratelimit.NewManager(zap.NewNop()), ratelimit.NewManager(zap.NewNop())
	a.SetSharedPool(testPool)
	b.SetSharedPool(testPool)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO api_rate_budgets(account_key,interval_ms) VALUES($1,30)`, key+":read"); err != nil {
		t.Fatal(err)
	}
	limiters := []*ratelimit.Tiny{a.GetOrCreateTiny(key), b.GetOrCreateTiny(key)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	times := make(chan time.Time, 6)
	errs := make(chan error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := limiters[i%2].WaitRequest(ctx, http.MethodGet)
			errs <- err
			times <- time.Now()
		}()
	}
	wg.Wait()
	close(times)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var dispatches []time.Time
	for now := range times {
		dispatches = append(dispatches, now)
	}
	sort.Slice(dispatches, func(i, j int) bool { return dispatches[i].Before(dispatches[j]) })
	if elapsed := dispatches[len(dispatches)-1].Sub(dispatches[0]); elapsed < 120*time.Millisecond {
		t.Fatalf("replicas exceeded shared schedule: six calls in %s", elapsed)
	}
}
func TestRateBudget_ReadLimitDoesNotBlockWriteCategory(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	m := ratelimit.NewManager(zap.NewNop())
	m.SetSharedPool(testPool)
	limiter := m.GetOrCreateTiny("test:" + fx.storeID)
	h := http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"2"}}
	if err := limiter.ObserveResponse(context.Background(), http.MethodGet, 429, h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := limiter.WaitRequest(ctx, http.MethodGet); !errors.Is(err, ratelimit.ErrNaoDespachado) {
		t.Fatalf("exhausted read budget: %v", err)
	}
	if err := limiter.WaitRequest(ctx, http.MethodPut); err != nil {
		t.Fatalf("read budget blocked a write: %v", err)
	}
}

func TestRateBudget_LearnsMinuteQuotaWithoutConfusingBurst(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	m := ratelimit.NewManager(zap.NewNop())
	m.SetSharedPool(testPool)
	key := "test:" + fx.storeID
	limiter := m.GetOrCreateTiny(key)
	ctx := context.Background()
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "60")
	h.Set("X-RateLimit-Remaining", "59")
	h.Set("X-RateLimit-Reset", "6")
	if err := limiter.ObserveResponse(ctx, http.MethodGet, 200, h); err != nil {
		t.Fatal(err)
	}
	h.Set("X-RateLimit-Limit", "4")
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", "1")
	if err := limiter.ObserveResponse(ctx, http.MethodGet, 429, h); err != nil {
		t.Fatal(err)
	}
	// A success without headers from another in-flight request must not
	// undo either the learned minute quota or the burst cooldown.
	if err := limiter.ObserveResponse(ctx, http.MethodGet, 200, nil); err != nil {
		t.Fatal(err)
	}
	var interval int64
	var blocked bool
	if err := testPool.QueryRow(ctx, `SELECT interval_ms,blocked_until>clock_timestamp() FROM api_rate_budgets WHERE account_key=$1`, key+":read").Scan(&interval, &blocked); err != nil {
		t.Fatal(err)
	}
	if interval != 1250 || !blocked {
		t.Fatalf("learned budget lost: interval=%d blocked=%v", interval, blocked)
	}
	if err := limiter.WaitRequest(ctx, http.MethodPut); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT interval_ms FROM api_rate_budgets WHERE account_key=$1`, key+":write").Scan(&interval); err != nil {
		t.Fatal(err)
	}
	if interval != 2500 {
		t.Fatalf("unknown write quota inherited read rate: %d", interval)
	}
}

func TestBlingRateBudgetSharesReadsWritesAndCooldownAcrossReplicas(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	key := "bling:test:" + fx.storeID
	a, b := ratelimit.NewManager(zap.NewNop()), ratelimit.NewManager(zap.NewNop())
	a.SetSharedPool(testPool)
	b.SetSharedPool(testPool)
	first, second := a.GetOrCreateBling(key, 2), b.GetOrCreateBling(key, 2)
	ctx := context.Background()
	if err := first.WaitRequest(ctx, http.MethodGet); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := second.WaitRequest(short, http.MethodPost); !errors.Is(err, ratelimit.ErrNaoDespachado) {
		t.Fatalf("replica write exceeded read quota: %v", err)
	}
	if err := second.BlockFor(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := first.ObserveResponse(ctx, http.MethodGet, 200, nil); err != nil {
		t.Fatal(err)
	}
	if err := first.WaitRequest(short, http.MethodGet); !errors.Is(err, ratelimit.ErrNaoDespachado) {
		t.Fatalf("old success cleared daily quota: %v", err)
	}
	var interval int64
	var blocked bool
	if err := testPool.QueryRow(ctx, `SELECT interval_ms,blocked_until>clock_timestamp()+interval '23 hours' FROM api_rate_budgets WHERE account_key=$1`, key).Scan(&interval, &blocked); err != nil {
		t.Fatal(err)
	}
	if interval != 500 || !blocked {
		t.Fatalf("shared budget lost: interval=%d blocked=%v", interval, blocked)
	}
	other := a.GetOrCreateBling(key+":other", 2)
	if err := other.Wait(ctx); err != nil {
		t.Fatalf("other account blocked: %v", err)
	}
}

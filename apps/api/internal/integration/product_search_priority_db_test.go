//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/lib/ratelimit"
)

func tinySearchReplicas(t *testing.T) (string, *ratelimit.Tiny, *ratelimit.Tiny) {
	t.Helper()
	requireDB(t)
	key := "tiny:search-test:" + uuid.NewString()
	first, second := ratelimit.NewManager(zap.NewNop()), ratelimit.NewManager(zap.NewNop())
	first.SetSharedPool(testPool)
	second.SetSharedPool(testPool)
	return key + ":read", first.GetOrCreateTiny(key), second.GetOrCreateTiny(key)
}

func TestProductSearchPrioritySharedFairTurn(t *testing.T) {
	key, first, second := tinySearchReplicas(t)
	ctx := t.Context()
	// Reproduce an aged, still-live background waiter while searches keep
	// renewing their lease. The shared quota itself is eligible for dispatch.
	_, err := testPool.Exec(ctx, `INSERT INTO api_rate_budgets
 (account_key,interval_ms,next_at,interactive_until,catalog_wait_since,catalog_until)
 VALUES($1,2500,clock_timestamp()-interval '1 second',clock_timestamp()+interval '5 seconds',
 clock_timestamp()-interval '11 seconds',clock_timestamp()+interval '5 seconds')`, key)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := first.WaitRequest(ratelimit.WithTinyInteractiveRead(short), http.MethodGet); !errors.Is(err, ratelimit.ErrNaoDespachado) {
		t.Fatalf("interactive replica stole background turn: %v", err)
	}
	if err := second.WaitRequest(ratelimit.WithTinyCatalogRead(short), http.MethodGet); err != nil {
		t.Fatalf("aged background replica did not get its slot: %v", err)
	}
	if err := first.WaitRequest(ratelimit.WithTinyInteractiveRead(short), http.MethodGet); !errors.Is(err, ratelimit.ErrNaoDespachado) {
		t.Fatalf("priority bypassed shared pacing: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE api_rate_budgets SET next_at=clock_timestamp()-interval '1 second'
 WHERE account_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := first.WaitRequest(ratelimit.WithTinyInteractiveRead(short), http.MethodGet); err != nil {
		t.Fatalf("next interactive lookup failed after background turn: %v", err)
	}
}

func TestProductSearchPriorityDoesNotBlockOperationalReads(t *testing.T) {
	key, first, second := tinySearchReplicas(t)
	ctx := t.Context()
	_, err := testPool.Exec(ctx, `INSERT INTO api_rate_budgets
 (account_key,interval_ms,next_at,interactive_until,catalog_wait_since,catalog_until)
 VALUES($1,2500,clock_timestamp()-interval '1 second',clock_timestamp()+interval '5 seconds',
 clock_timestamp()-interval '11 seconds',clock_timestamp()+interval '5 seconds')`, key)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := first.WaitRequest(short, http.MethodGet); err != nil {
		t.Fatalf("comment/payment read was held behind background work: %v", err)
	}
	if err := second.WaitRequest(ratelimit.WithTinyCatalogRead(short), http.MethodPut); err != nil {
		t.Fatalf("read fairness blocked independent write quota: %v", err)
	}
	_, other, _ := tinySearchReplicas(t)
	if err := other.WaitRequest(ratelimit.WithTinyInteractiveRead(short), http.MethodGet); err != nil {
		t.Fatalf("another merchant inherited pending quota: %v", err)
	}
}

func TestProductSearchPriorityCanceledWaitDoesNotConsumeSharedSlot(t *testing.T) {
	key, first, second := tinySearchReplicas(t)
	ctx := t.Context()
	var nextAt time.Time
	err := testPool.QueryRow(ctx, `INSERT INTO api_rate_budgets(account_key,interval_ms,next_at)
 VALUES($1,2500,clock_timestamp()+interval '3 seconds') RETURNING next_at`, key).Scan(&nextAt)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- first.WaitRequest(ratelimit.WithTinyInteractiveRead(waitCtx), http.MethodGet) }()
	// Synchronize with the actual committed claim, rather than guessing when
	// the replica has started waiting using a fixed sleep.
	readyCtx, readyCancel := context.WithTimeout(ctx, time.Second)
	defer readyCancel()
	for {
		var registered bool
		if err := testPool.QueryRow(readyCtx, `SELECT interactive_until>clock_timestamp()
 FROM api_rate_budgets WHERE account_key=$1`, key).Scan(&registered); err != nil {
			t.Fatal(err)
		}
		if registered {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost while queued: %v", err)
	}
	var after time.Time
	if err := testPool.QueryRow(ctx, `SELECT next_at FROM api_rate_budgets WHERE account_key=$1`, key).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(nextAt) {
		t.Fatalf("cancelled wait reserved future capacity: before=%s after=%s", nextAt, after)
	}
	if _, err := testPool.Exec(ctx, `UPDATE api_rate_budgets SET next_at=clock_timestamp()-interval '1 second'
 WHERE account_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := second.WaitRequest(ctx, http.MethodGet); err != nil {
		t.Fatalf("next operational read could not reuse quota: %v", err)
	}
}

func TestProductSearchPriorityExpiredBackgroundLeaseDoesNotBlockLookup(t *testing.T) {
	key, first, _ := tinySearchReplicas(t)
	ctx := t.Context()
	_, err := testPool.Exec(ctx, `INSERT INTO api_rate_budgets
 (account_key,interval_ms,next_at,catalog_wait_since,catalog_until)
 VALUES($1,2500,clock_timestamp()-interval '1 second',clock_timestamp()-interval '20 seconds',
 clock_timestamp()-interval '1 second')`, key)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := first.WaitRequest(ratelimit.WithTinyInteractiveRead(short), http.MethodGet); err != nil {
		t.Fatalf("abandoned background read kept the quota hostage: %v", err)
	}
}

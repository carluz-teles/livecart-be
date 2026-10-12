package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
)

func quotaHeaders(limit, remaining, reset string) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", limit)
	h.Set("X-RateLimit-Remaining", remaining)
	h.Set("X-RateLimit-Reset", reset)
	return h
}

func TestTinyEnforcesQuotaWithoutHeaders(t *testing.T) {
	limiter := NewManager(zap.NewNop()).GetOrCreateTiny("account")
	now := time.Now()
	limiter.now = func() time.Time { return now }
	ctx := context.Background()
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		key := limiter.category(method)
		for range 24 {
			res, err := limiter.claim(ctx, key)
			if err != nil || !res.Allowed {
				t.Fatalf("expected evenly spaced dispatch: %+v %v", res, err)
			}
			if err := limiter.ObserveResponse(ctx, method, 200, nil); err != nil {
				t.Fatal(err)
			}
			res, err = limiter.claim(ctx, key)
			if err != nil || res.Allowed || res.RetryAfter != 2500*time.Millisecond {
				t.Fatalf("missing headers removed the quota: %+v %v", res, err)
			}
			now = now.Add(2500 * time.Millisecond)
		}
	}
}

func TestTinyInteractiveReadsPrecedeCatalogWithoutBypassingQuota(t *testing.T) {
	limiter := NewManager(zap.NewNop()).GetOrCreateTiny("account")
	now := time.Now()
	limiter.now = func() time.Time { return now }
	interactive := WithTinyInteractiveRead(t.Context())
	bulk := WithTinyCatalogRead(t.Context())
	key := limiter.category(http.MethodGet)
	first, err := limiter.claim(interactive, key)
	if err != nil || !first.Allowed {
		t.Fatalf("first interactive read: %+v %v", first, err)
	}
	now = now.Add(2500 * time.Millisecond)
	background, _ := limiter.claim(bulk, key)
	if background.Allowed {
		t.Fatal("catalogue took the interactive slot")
	}
	second, _ := limiter.claim(interactive, key)
	if !second.Allowed {
		t.Fatal("interactive read blocked by catalogue lease")
	}
	tooSoon, _ := limiter.claim(interactive, key)
	if tooSoon.Allowed || tooSoon.RetryAfter != 2500*time.Millisecond {
		t.Fatalf("interactive priority bypassed quota: %+v", tooSoon)
	}
	write, _ := limiter.claim(bulk, limiter.category(http.MethodPut))
	if !write.Allowed {
		t.Fatal("read lease delayed an independent write")
	}
	now = now.Add(2500 * time.Millisecond)
	order, _ := limiter.claim(t.Context(), key)
	if !order.Allowed {
		t.Fatal("catalogue preference blocked an order operation")
	}
	now = now.Add(tinyInteractiveLease)
	resumed, _ := limiter.claim(bulk, key)
	if !resumed.Allowed {
		t.Fatal("abandoned interactive operation blocked catalogue indefinitely")
	}
}

func TestTinyInteractivePriorityStillHonorsCooldownAndCancellation(t *testing.T) {
	limiter := NewManager(zap.NewNop()).GetOrCreateTiny("account")
	ctx := WithTinyInteractiveRead(t.Context())
	if err := limiter.ObserveResponse(ctx, http.MethodGet, 429, quotaHeaders("60", "0", "60")); err != nil {
		t.Fatal(err)
	}
	res, err := limiter.claim(ctx, limiter.category(http.MethodGet))
	if err != nil || res.Allowed || res.RetryAfter < 59*time.Second {
		t.Fatalf("interactive request bypassed cooldown: %+v %v", res, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := limiter.WaitRequest(cancelled, http.MethodGet); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled interactive request kept waiting: %v", err)
	}
}

func TestTinyQueuedCatalogYieldsToInteractiveAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewManager(zap.NewNop()).GetOrCreateTiny("queued")
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := limiter.WaitRequest(ctx, http.MethodGet); err != nil {
			t.Fatal(err)
		}
		background := make(chan error, 3)
		for range 3 {
			go func() { background <- limiter.WaitRequest(WithTinyCatalogRead(ctx), http.MethodGet) }()
		}
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		if err := limiter.WaitRequest(WithTinyInteractiveRead(ctx), http.MethodGet); err != nil {
			t.Fatalf("interactive lookup behind queued catalogue: %v", err)
		}
		select {
		case err := <-background:
			t.Fatalf("catalogue dispatched before the interactive lookup: %v", err)
		default:
		}
		last := time.Now()
		for range 3 {
			if err := <-background; err != nil {
				t.Fatalf("catalogue did not resume: %v", err)
			}
			if elapsed := time.Since(last); elapsed < 2500*time.Millisecond {
				t.Fatalf("shared quota exceeded after lookup: %s", elapsed)
			}
			last = time.Now()
		}
	})
}

func TestTinyContinuousInteractiveTrafficAllowsBackgroundTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewManager(zap.NewNop()).GetOrCreateTiny("fair")
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		interactive := WithTinyInteractiveRead(ctx)
		if err := limiter.WaitRequest(interactive, http.MethodGet); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		type dispatch struct {
			at  time.Time
			err error
		}
		background := make(chan dispatch, 1)
		go func() {
			err := limiter.WaitRequest(WithTinyCatalogRead(ctx), http.MethodGet)
			background <- dispatch{at: time.Now(), err: err}
		}()
		synctest.Wait()
		// A busy screen repeatedly renews its priority while the stock reader is
		// queued. It must still yield one slot after the bounded priority wait.
		for time.Since(started) < tinyCatalogMaxPriorityWait+3*time.Second {
			if err := limiter.WaitRequest(interactive, http.MethodGet); err != nil && !errors.Is(err, ErrNaoDespachado) {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		select {
		case result := <-background:
			if result.err != nil {
				t.Fatalf("background fairness lost: %v", result.err)
			}
			if elapsed := result.at.Sub(started); elapsed > tinyCatalogMaxPriorityWait+2600*time.Millisecond {
				t.Fatalf("background turn was delayed past the next eligible quota slot: %s", elapsed)
			}
		default:
			t.Fatal("continuous searches starved stock recovery")
		}
		if err := limiter.WaitRequest(interactive, http.MethodGet); err != nil {
			t.Fatalf("interactive lookup did not recover after background turn: %v", err)
		}
	})
}

func TestTinyCanceledWaitDoesNotReserveFutureCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewManager(zap.NewNop()).GetOrCreateTiny("cancelled")
		if err := limiter.WaitRequest(t.Context(), http.MethodGet); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- limiter.WaitRequest(WithTinyInteractiveRead(ctx), http.MethodGet) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait: %v", err)
		}
		time.Sleep(2500 * time.Millisecond)
		if res, err := limiter.claim(t.Context(), limiter.category(http.MethodGet)); err != nil || !res.Allowed {
			t.Fatalf("cancelled waiter wasted next slot: %+v %v", res, err)
		}
	})
}

func TestTinyInteractiveQuotaWaitIsBoundedAndDoesNotRenewImpossibleLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewManager(zap.NewNop()).GetOrCreateTiny("bounded")
		ctx := t.Context()
		if err := limiter.ObserveResponse(ctx, http.MethodGet, 429, quotaHeaders("30", "0", "60")); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err := limiter.WaitRequest(WithTinyInteractiveRead(ctx), http.MethodGet); !errors.Is(err, ErrNaoDespachado) {
			t.Fatalf("expected safe refusal before dispatch: %v", err)
		}
		if time.Since(started) > tinyInteractiveMaxQuotaWait {
			t.Fatal("interactive quota wait exceeded its budget")
		}
		budget := limiter.localBudget(limiter.category(http.MethodGet))
		if !budget.interactiveUntil.IsZero() || !budget.nextAt.IsZero() {
			t.Fatalf("impossible lookup changed priority/capacity: %+v", budget)
		}
		time.Sleep(time.Minute)
		if err := limiter.WaitRequest(WithTinyCatalogRead(ctx), http.MethodGet); err != nil {
			t.Fatal(err)
		}
		if err := limiter.WaitRequest(WithTinyInteractiveRead(ctx), http.MethodGet); err != nil {
			t.Fatalf("next lookup did not recover after cooldown: %v", err)
		}
	})
}

func TestTinyFairTurnDoesNotDelayOrderReadsOrOtherAccounts(t *testing.T) {
	limiter := NewManager(zap.NewNop()).GetOrCreateTiny("fair-normal")
	now := time.Now()
	limiter.now = func() time.Time { return now }
	key := limiter.category(http.MethodGet)
	budget := limiter.localBudget(key)
	budget.catalogWaitSince = now.Add(-tinyCatalogMaxPriorityWait)
	budget.catalogUntil = now.Add(tinyInteractiveLease)
	if res, err := limiter.claim(WithTinyInteractiveRead(t.Context()), key); err != nil || res.Allowed {
		t.Fatalf("lookup stole the background turn: %+v %v", res, err)
	}
	if res, err := limiter.claim(t.Context(), key); err != nil || !res.Allowed {
		t.Fatalf("background turn blocked an order/comment read: %+v %v", res, err)
	}
	otherKey := "other:read"
	if res, err := limiter.claim(WithTinyInteractiveRead(t.Context()), otherKey); err != nil || !res.Allowed {
		t.Fatalf("priority leaked into another account: %+v %v", res, err)
	}
	now = now.Add(tinyInteractiveLease)
	if res, err := limiter.claim(WithTinyInteractiveRead(t.Context()), key); err != nil || !res.Allowed {
		t.Fatalf("abandoned background lease blocked a lookup: %+v %v", res, err)
	}
}

func TestTinyImpossiblePriorityWaitDoesNotLeavePendingReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewManager(zap.NewNop()).GetOrCreateTiny("impossible-priority")
		key := limiter.category(http.MethodGet)
		budget := limiter.localBudget(key)
		budget.catalogWaitSince = time.Now().Add(-tinyCatalogMaxPriorityWait)
		budget.catalogUntil = time.Now().Add(tinyInteractiveLease)
		before := *budget
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if err := limiter.WaitRequest(WithTinyInteractiveRead(ctx), http.MethodGet); !errors.Is(err, ErrNaoDespachado) {
			t.Fatalf("expected refusal without dispatch: %v", err)
		}
		if *budget != before {
			t.Fatalf("refused search renewed priority or spent capacity: before=%+v after=%+v", before, budget)
		}
	})
}

func TestTinyBurstAndOldSuccessCannotClearCooldown(t *testing.T) {
	limiter := NewManager(zap.NewNop()).GetOrCreateTiny("account")
	now := time.Now()
	limiter.now = func() time.Time { return now }
	ctx := context.Background()
	observe := func(status int, h http.Header) {
		t.Helper()
		if err := limiter.ObserveResponse(ctx, http.MethodGet, status, h); err != nil {
			t.Fatal(err)
		}
	}
	// Actual /info headers from the incident follow-up: 60 reads per minute.
	observe(200, quotaHeaders("60", "59", "6"))
	observe(429, quotaHeaders("4", "0", "1"))
	observe(200, quotaHeaders("60", "59", "6"))
	res, _ := limiter.claim(ctx, limiter.category(http.MethodGet))
	if res.Allowed || res.RetryAfter != time.Second {
		t.Fatalf("cooldown lost: %+v", res)
	}
	write, _ := limiter.Allow(ctx)
	if !write.Allowed {
		t.Fatal("read 429 blocked a write")
	}
	now = now.Add(time.Second)
	res, _ = limiter.claim(ctx, limiter.category(http.MethodGet))
	if !res.Allowed {
		t.Fatalf("burst wait interpreted as minute quota: %+v", res)
	}
	res, _ = limiter.claim(ctx, limiter.category(http.MethodGet))
	if res.RetryAfter != 1250*time.Millisecond {
		t.Fatalf("lost learned 48/min pace: %+v", res)
	}
}

func TestTinyMinuteCooldownWithoutHeadersAndCancelledWait(t *testing.T) {
	limiter := NewManager(zap.NewNop()).GetOrCreateTiny("account")
	if err := limiter.ObserveResponse(context.Background(), http.MethodGet, 429, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := limiter.WaitRequest(ctx, http.MethodGet); !errors.Is(err, ErrNaoDespachado) {
		t.Fatalf("request should be refused before dispatch: %v", err)
	}
	res, err := limiter.claim(context.Background(), limiter.category(http.MethodGet))
	if err != nil || res.RetryAfter < 59*time.Second || res.RetryAfter > time.Minute {
		t.Fatalf("cancelled caller changed the cooldown: %+v %v", res, err)
	}
}

func TestRetryAfterHonorsMinuteWindowAndHTTPDate(t *testing.T) {
	h := quotaHeaders("30", "0", "58")
	h.Set("Retry-After", "1")
	if got := RetryAfter(h, time.Second); got != 58*time.Second {
		t.Fatalf("wait shortened to %s", got)
	}
	h = http.Header{}
	h.Set("Retry-After", time.Now().Add(20*time.Second).UTC().Format(http.TimeFormat))
	if got := RetryAfter(h, time.Second); got < 18*time.Second || got > 20*time.Second {
		t.Fatalf("HTTP date ignored: %s", got)
	}
}

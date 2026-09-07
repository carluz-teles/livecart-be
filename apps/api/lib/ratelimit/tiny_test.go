package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"testing"
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

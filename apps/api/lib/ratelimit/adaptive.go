package ratelimit

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// AdaptiveLimiter implements RateLimiter using uniform throttling based on
// X-RateLimit-* headers from the external API.
//
// How it works:
//   - First request: passes through (no data yet, 1 call never exceeds limits)
//   - After first response: API headers provide Remaining and Reset values
//   - Subsequent requests: spaced uniformly as interval = resetSeconds / remaining
//   - On 429: stops all requests until X-RateLimit-Reset expires
//
// This approach auto-calibrates with real API state, respects other apps
// sharing the same account quota, and requires zero configuration.
type AdaptiveLimiter struct {
	mu sync.Mutex

	// API state (updated from response headers)
	remaining  int       // X-RateLimit-Remaining
	resetAt    time.Time // when the rate limit window resets
	hasAPIData bool      // true after first response with headers

	// Throttling control
	lastRequest time.Time // when the last request was made

	logger *zap.Logger
}

// NewAdaptiveLimiter creates a new adaptive rate limiter.
func NewAdaptiveLimiter(logger *zap.Logger) *AdaptiveLimiter {
	return &AdaptiveLimiter{
		logger: logger,
	}
}

// Allow checks if the next request is permitted based on current API state.
func (l *AdaptiveLimiter) Allow(ctx context.Context) (*Reservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// No API data yet (first call) — allow it, the response will bring headers
	if !l.hasAPIData {
		l.lastRequest = now
		return &Reservation{Allowed: true, Remaining: -1}, nil
	}

	// Reset window has passed — clear state and allow
	if now.After(l.resetAt) {
		l.hasAPIData = false
		l.lastRequest = now
		return &Reservation{Allowed: true, Remaining: -1}, nil
	}

	// No remaining requests — must wait for reset
	if l.remaining <= 0 {
		retryAfter := l.resetAt.Sub(now)
		return &Reservation{
			Allowed:    false,
			RetryAfter: retryAfter,
			Remaining:  0,
		}, nil
	}

	// Calculate uniform interval: distribute remaining requests over remaining time
	timeToReset := l.resetAt.Sub(now)
	interval := timeToReset / time.Duration(l.remaining)

	// Check if enough time has passed since last request
	elapsed := now.Sub(l.lastRequest)
	if elapsed < interval {
		waitTime := interval - elapsed
		return &Reservation{
			Allowed:    false,
			RetryAfter: waitTime,
			Remaining:  l.remaining,
		}, nil
	}

	// Allowed — consume one request
	l.remaining--
	l.lastRequest = now
	return &Reservation{
		Allowed:   true,
		Remaining: l.remaining,
	}, nil
}

// Wait blocks until the next request is permitted or ctx is cancelled.
func (l *AdaptiveLimiter) Wait(ctx context.Context) error {
	for {
		res, err := l.Allow(ctx)
		if err != nil {
			return err
		}

		if res.Allowed {
			return nil
		}

		l.logger.Debug("rate limit: throttling request",
			zap.Duration("wait", res.RetryAfter),
			zap.Int("remaining", res.Remaining),
		)

		timer := time.NewTimer(res.RetryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Try again after waiting
		}
	}
}

// UpdateFromHeaders updates the limiter with real data from API response headers.
// Should be called after every HTTP response that contains X-RateLimit-* headers.
func (l *AdaptiveLimiter) UpdateFromHeaders(remaining int, resetSeconds int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.remaining = remaining
	l.resetAt = time.Now().Add(time.Duration(resetSeconds) * time.Second)
	l.hasAPIData = true
}

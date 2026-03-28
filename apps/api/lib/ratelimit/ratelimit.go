package ratelimit

import (
	"context"
	"fmt"
	"time"
)

// RateLimiter controls the rate of outbound API calls for a specific integration.
// It uses headers from the external API (X-RateLimit-Remaining, X-RateLimit-Reset)
// to adaptively throttle requests, distributing them uniformly over time.
type RateLimiter interface {
	// Allow checks if the next request is permitted.
	// Returns a Reservation indicating whether to proceed or wait.
	Allow(ctx context.Context) (*Reservation, error)

	// Wait blocks until the next request is permitted or ctx is cancelled.
	Wait(ctx context.Context) error

	// UpdateFromHeaders updates the limiter state with real data from the API.
	// Called after each HTTP response that contains X-RateLimit-* headers.
	UpdateFromHeaders(remaining int, resetSeconds int)
}

// Reservation is the result of an Allow() call.
type Reservation struct {
	Allowed    bool
	RetryAfter time.Duration // how long to wait if !Allowed
	Remaining  int           // requests remaining in current cycle
}

// ErrRateLimited is returned when a request cannot proceed due to rate limiting.
type ErrRateLimited struct {
	RetryAfter time.Duration
}

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("rate limited: retry after %s", e.RetryAfter)
}

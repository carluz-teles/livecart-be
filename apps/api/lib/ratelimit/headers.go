package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfter returns the longest valid wait advertised by the provider.
// Tiny's X-RateLimit-Reset is a duration in seconds, not a Unix timestamp.
func RetryAfter(headers http.Header, fallback time.Duration) time.Duration {
	var wait time.Duration
	for _, key := range []string{"X-RateLimit-Reset", "RateLimit-Reset", "Retry-After"} {
		value := strings.TrimSpace(headers.Get(key))
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 300 {
			wait = max(wait, time.Duration(seconds)*time.Second)
			continue
		}
		if key == "Retry-After" {
			if at, err := http.ParseTime(value); err == nil {
				if delay := time.Until(at); delay > 0 && delay <= 5*time.Minute {
					wait = max(wait, delay)
				}
			}
		}
	}
	if wait > 0 {
		return wait
	}
	return fallback
}

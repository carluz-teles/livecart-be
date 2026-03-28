package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/ratelimit"
)

// BaseProvider provides common functionality for all providers.
type BaseProvider struct {
	IntegrationID string
	StoreID       string
	Logger        *zap.Logger
	HTTPClient    *http.Client
	LogFunc       LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// LogFunc is a function that logs integration operations.
type LogFunc func(ctx context.Context, log IntegrationLog) error

// IntegrationLog represents an integration operation log entry.
type IntegrationLog struct {
	IntegrationID   string
	EntityType      string
	EntityID        string
	Direction       string // "outbound" or "inbound"
	Status          string // "success" or "error"
	RequestPayload  []byte
	ResponsePayload []byte
	ErrorMessage    string
}

// BaseProviderConfig contains configuration for creating a BaseProvider.
type BaseProviderConfig struct {
	IntegrationID string
	StoreID       string
	Logger        *zap.Logger
	LogFunc       LogFunc
	Timeout       time.Duration
	RateLimiter   ratelimit.RateLimiter
}

// NewBaseProvider creates a new BaseProvider with the given configuration.
func NewBaseProvider(cfg BaseProviderConfig) *BaseProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &BaseProvider{
		IntegrationID: cfg.IntegrationID,
		StoreID:       cfg.StoreID,
		Logger:        cfg.Logger,
		LogFunc:       cfg.LogFunc,
		RateLimiter:   cfg.RateLimiter,
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// DoRequest performs an HTTP request with logging and rate limiting.
func (b *BaseProvider) DoRequest(ctx context.Context, method, url string, body any, headers map[string]string) (*http.Response, []byte, error) {
	// Throttle request based on API rate limit headers
	if b.RateLimiter != nil {
		if err := b.RateLimiter.Wait(ctx); err != nil {
			return nil, nil, err
		}
	}

	var reqBody []byte
	var err error

	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling request body: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	startTime := time.Now()
	resp, err := b.HTTPClient.Do(req)
	duration := time.Since(startTime)

	b.Logger.Debug("http request",
		zap.String("integration_id", b.IntegrationID),
		zap.String("method", method),
		zap.String("url", url),
		zap.Duration("duration", duration),
	)

	if err != nil {
		b.logOperation(ctx, IntegrationLog{
			IntegrationID:  b.IntegrationID,
			Direction:      "outbound",
			Status:         "error",
			RequestPayload: reqBody,
			ErrorMessage:   err.Error(),
		})
		return nil, nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response body: %w", err)
	}

	status := "success"
	var errorMsg string
	if resp.StatusCode >= 400 {
		status = "error"
		errorMsg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	b.logOperation(ctx, IntegrationLog{
		IntegrationID:   b.IntegrationID,
		Direction:       "outbound",
		Status:          status,
		RequestPayload:  reqBody,
		ResponsePayload: respBody,
		ErrorMessage:    errorMsg,
	})

	// Update rate limiter with real API data from response headers
	if b.RateLimiter != nil {
		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
			rem, _ := strconv.Atoi(remaining)
			reset, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset"))
			b.RateLimiter.UpdateFromHeaders(rem, reset)
		}
	}

	return resp, respBody, nil
}

// DoRequestWithRetry performs a request with exponential backoff retry.
func (b *BaseProvider) DoRequestWithRetry(ctx context.Context, maxRetries int, method, url string, body any, headers map[string]string) (*http.Response, []byte, error) {
	var lastResp *http.Response
	var lastBody []byte
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(100<<uint(attempt-1)) * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}

			b.Logger.Debug("retrying request",
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
			)

			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		resp, respBody, err := b.DoRequest(ctx, method, url, body, headers)
		if err != nil {
			lastErr = err
			continue
		}

		// Retry on 429 Too Many Requests — wait for the reset period
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := 60 // default 60s if no header
			if ra := resp.Header.Get("X-RateLimit-Reset"); ra != "" {
				if parsed, err := strconv.Atoi(ra); err == nil && parsed > 0 {
					retryAfter = parsed
				}
			} else if ra := resp.Header.Get("Retry-After"); ra != "" {
				if parsed, err := strconv.Atoi(ra); err == nil && parsed > 0 {
					retryAfter = parsed
				}
			}

			b.Logger.Warn("rate limited by API (429), waiting for reset",
				zap.String("integration_id", b.IntegrationID),
				zap.Int("retry_after_seconds", retryAfter),
			)

			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Duration(retryAfter) * time.Second):
			}

			lastResp = resp
			lastBody = respBody
			lastErr = &ratelimit.ErrRateLimited{RetryAfter: time.Duration(retryAfter) * time.Second}
			continue
		}

		// Don't retry client errors (4xx), only server errors (5xx)
		if resp.StatusCode < 500 {
			return resp, respBody, nil
		}

		lastResp = resp
		lastBody = respBody
		lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
	}

	if lastErr != nil {
		return lastResp, lastBody, fmt.Errorf("max retries exceeded: %w", lastErr)
	}
	return lastResp, lastBody, nil
}

// logOperation logs an integration operation if LogFunc is set.
func (b *BaseProvider) logOperation(ctx context.Context, log IntegrationLog) {
	if b.LogFunc == nil {
		return
	}

	if err := b.LogFunc(ctx, log); err != nil {
		b.Logger.Warn("failed to log integration operation",
			zap.String("integration_id", log.IntegrationID),
			zap.Error(err),
		)
	}
}

// ParseResponse is a helper to parse JSON response into a struct.
func ParseResponse[T any](body []byte) (*T, error) {
	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &result, nil
}

// IsSuccessStatus checks if the HTTP status code indicates success.
func IsSuccessStatus(statusCode int) bool {
	return statusCode >= 200 && statusCode < 300
}

package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
)

type blingOAuthErro struct {
	status int
	code   string
}

func (e *blingOAuthErro) Error() string {
	return fmt.Sprintf("bling: token endpoint HTTP %d (%s)", e.status, e.code)
}
func (e *blingOAuthErro) Status() int { return e.status }
func (e *blingOAuthErro) Permanent() bool {
	return e.status >= 400 && e.status < 500 &&
		(e.code == "invalid_grant" || e.code == "invalid_client" || e.code == "unauthorized_client")
}

// Never include the token endpoint response in errors: it may echo credentials.
func novoBlingOAuthErro(status int, body []byte) error {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	var code string
	if json.Unmarshal(env.Error, &code) != nil {
		var nested struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(env.Error, &nested)
		code = nested.Type
	}
	switch code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_request", "TOO_MANY_REQUESTS", "SERVER_ERROR":
	default:
		code = "oauth_error"
	}
	return &blingOAuthErro{status: status, code: code}
}

// Only repeat a write explicitly refused with 429. A network failure or 5xx
// may follow an applied write and must be reconciled by its order marker.
func (b *Bling) request(ctx context.Context, method, endpoint string, body any) (*http.Response, []byte, error) {
	for attempt := 0; ; attempt++ {
		resp, raw, err := b.DoRequest(ctx, method, endpoint, body, b.authHeaders())
		if err != nil || resp.StatusCode != http.StatusTooManyRequests {
			return resp, raw, err
		}
		var env struct {
			Error struct {
				Period string `json:"period"`
				Limit  int    `json:"limit"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		wait := ratelimit.RetryAfter(resp.Header, 2*time.Second)
		if env.Error.Period == "day" {
			// The API says to retry tomorrow but publishes no exact reset time.
			// A full day is conservative and avoids repeatedly hitting an exhausted quota.
			if wait < 24*time.Hour {
				wait = 24 * time.Hour
			}
		}
		if limiter, ok := b.RateLimiter.(interface {
			BlockFor(context.Context, time.Duration) error
		}); ok {
			if blockErr := limiter.BlockFor(ctx, wait); blockErr != nil {
				return resp, raw, fmt.Errorf("bling: persistindo pausa de rate limit: %w", blockErr)
			}
		}
		logger.From(ctx, b.Logger).Warn("bling API quota exceeded",
			zap.String("integration_id", b.IntegrationID), zap.String("method", method),
			zap.String("period", env.Error.Period), zap.Int("limit", env.Error.Limit),
			zap.Duration("retry_after", wait), zap.Int("attempt", attempt+1))
		if attempt >= 2 || env.Error.Period == "day" {
			return resp, raw, nil
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
			return resp, raw, nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return resp, raw, ctx.Err()
		case <-timer.C:
		}
	}
}

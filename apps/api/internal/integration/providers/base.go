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

	"livecart/apps/api/lib/logger"
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
		quotaStarted := time.Now()
		var waitErr error
		if limiter, ok := b.RateLimiter.(interface {
			WaitRequest(context.Context, string) error
		}); ok {
			waitErr = limiter.WaitRequest(ctx, method)
		} else {
			waitErr = b.RateLimiter.Wait(ctx)
		}
		if elapsed := time.Since(quotaStarted); elapsed >= time.Second || waitErr != nil {
			logger.From(ctx, b.Logger).Info("provider request quota wait",
				zap.String("store_id", b.StoreID), zap.String("integration_id", b.IntegrationID),
				zap.String("method", method), zap.Duration("quota_wait", elapsed), zap.Error(waitErr))
		}
		if err := waitErr; err != nil {
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

	logger.From(ctx, b.Logger).Debug("http request",
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

	// One event per refused HTTP request, with method and endpoint. Retry-loop
	// messages alone cannot count requests or distinguish reads from writes.
	if resp.StatusCode == http.StatusTooManyRequests {
		logger.From(ctx, b.Logger).Warn("provider HTTP 429",
			zap.String("integration_id", b.IntegrationID),
			zap.String("method", method),
			zap.String("path", req.URL.EscapedPath()),
			zap.String("rate_limit", resp.Header.Get("X-RateLimit-Limit")),
			zap.String("rate_remaining", resp.Header.Get("X-RateLimit-Remaining")),
			zap.String("rate_reset", resp.Header.Get("X-RateLimit-Reset")),
			zap.String("retry_after", resp.Header.Get("Retry-After")),
		)
	}

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

	if limiter, ok := b.RateLimiter.(interface {
		ObserveResponse(context.Context, string, int, http.Header) error
	}); ok {
		if err := limiter.ObserveResponse(ctx, method, resp.StatusCode, resp.Header); err != nil {
			b.Logger.Warn("persisting API budget", zap.Error(err))
		}
		return resp, respBody, nil
	}
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

			logger.From(ctx, b.Logger).Debug("retrying request",
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

			logger.From(ctx, b.Logger).Warn("rate limited by API (429), waiting for reset",
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
		logger.From(ctx, b.Logger).Warn("failed to log integration operation",
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

// resetDoRateLimit lê quantos segundos faltam para a janela de rate limit rolar.
//
// Medido contra a API v3 do Tiny em 25/08/2026: são dois baldes — 4 requisições
// por segundo (429 com `X-Ratelimit-Limit: 4`, reset 1) e 30 por minuto (429 com
// limite 30, reset até 58). NÃO existe header `Retry-After` ali; `Retry-After`
// fica como fallback para outros provedores.
func resetDoRateLimit(h http.Header, padrao time.Duration) time.Duration {
	return ratelimit.RetryAfter(h, padrao)
}

// DoRequestRetrying429 repete a requisição APENAS quando a API recusa por rate
// limit. É o retry das ESCRITAS, e a diferença para DoRequestWithRetry é
// deliberada: um 5xx numa escrita significa que o servidor respondeu e pode ter
// aplicado, então repetir duplicaria; um 429 é recusa ANTES de aplicar, provado
// não-aplicado, e repetir é a única cura.
//
// Só espera se a janela couber no prazo restante do contexto. Estourar o
// deadline dormindo deixaria a escrita sem desfecho registrado — pior do que
// devolver o 429 para quem sabe reagendar (o razão de movimentos, o outbox).
// Por isso o 429 final volta como resposta normal, não como erro: o chamador
// decide o que ele significa no seu fluxo.
func (b *BaseProvider) DoRequestRetrying429(ctx context.Context, maxRetries int, method, url string, body any, headers map[string]string) (*http.Response, []byte, error) {
	for tentativa := 0; ; tentativa++ {
		resp, respBody, err := b.DoRequest(ctx, method, url, body, headers)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || tentativa >= maxRetries {
			return resp, respBody, err
		}

		espera := resetDoRateLimit(resp.Header, 2*time.Second)

		// Sem deadline não há o que estourar; com deadline, só dorme se sobrar
		// folga depois da espera.
		if prazo, temPrazo := ctx.Deadline(); temPrazo {
			if restante := time.Until(prazo); restante <= espera {
				logger.From(ctx, b.Logger).Warn("rate limited request: reset exceeds remaining deadline",
					zap.String("integration_id", b.IntegrationID),
					zap.String("method", method),
					zap.Duration("reset_in", espera),
					zap.Duration("deadline_in", restante),
				)
				return resp, respBody, nil
			}
		}

		logger.From(ctx, b.Logger).Warn("rate limited request (429): waiting for reset",
			zap.String("integration_id", b.IntegrationID),
			zap.String("method", method),
			zap.Int("attempt", tentativa+1),
			zap.Duration("reset_in", espera),
		)

		select {
		case <-ctx.Done():
			return resp, respBody, ctx.Err()
		case <-time.After(espera):
		}
	}
}

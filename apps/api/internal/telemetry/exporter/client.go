// Package exporter forwards select canonical LiveCart facts (event/session
// lifecycle, cart payment) to New Relic as custom events, so the telemetry
// dashboard project (docs/event-audit.md) has data to query. It is a thin
// consumer of the events package (internal/events): it never imports domain
// packages, it only decodes envelopes it already receives.
package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// insightsEventsURLFormat is the New Relic Insights Event API endpoint. See
// https://docs.newrelic.com/docs/data-apis/ingest-apis/event-api/introduction-event-api/
const insightsEventsURLFormat = "https://insights-collector.newrelic.com/v1/accounts/%s/events"

// requestTimeout bounds a single New Relic POST attempt. Telemetry export must
// never stall the asynq consumer pool waiting on New Relic.
const requestTimeout = 5 * time.Second

// maxRetries is deliberately small — this is best-effort telemetry, not a
// business-critical write; a New Relic outage should fail fast and log, not
// hold up the worker pool.
const maxRetries = 2

// NRClient posts New Relic custom events to the Insights Event API. It embeds
// providers.BaseProvider — the same HTTP client wrapper every payment/ERP
// provider (e.g. internal/integration/providers/payment/pagarme.go) already
// uses — to reuse its DoRequestWithRetry exponential backoff instead of
// hand-rolling a new retry loop.
type NRClient struct {
	*providers.BaseProvider
	url    string
	apiKey string
}

// NewNRClient builds an NRClient targeting cfg.AccountID with cfg.APIKey as
// the ingest key. Safe to construct even when cfg.Enabled is false — callers
// (Listeners) gate every SendEvent behind cfg.Enabled, so a disabled client
// simply never gets called.
func NewNRClient(cfg Config, log *zap.Logger) *NRClient {
	return &NRClient{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: "telemetry-new-relic-exporter",
			Logger:        log,
			Timeout:       requestTimeout,
		}),
		url:    fmt.Sprintf(insightsEventsURLFormat, cfg.AccountID),
		apiKey: cfg.APIKey,
	}
}

// SendEvent POSTs a single New Relic custom event as a one-item batch (the
// Insights Event API always expects a JSON array). event is typically one of
// the payloads.go structs, which each carry their own "eventType" field.
func (c *NRClient) SendEvent(ctx context.Context, event any) error {
	return c.SendEvents(ctx, []any{event})
}

// SendEvents keeps a sale's cart, payment and items in the same request. Split
// large batches below 100 KB, well within the Event API's 1 MB hard limit.
// The caller's deadline bounds the entire dispatch, including retries.
func (c *NRClient) SendEvents(ctx context.Context, events []any) error {
	const maxBatchBytes = 100_000
	batch := []json.RawMessage{}
	size := 2 // JSON array delimiters.
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encoding new relic event: %w", err)
		}
		if len(encoded)+2 > maxBatchBytes {
			return fmt.Errorf("new relic event exceeds batch size limit")
		}
		if len(batch) > 0 && size+len(encoded)+1 > maxBatchBytes {
			if err := c.sendBatch(ctx, batch); err != nil {
				return err
			}
			batch = []json.RawMessage{}
			size = 2
		}
		batch = append(batch, encoded)
		size += len(encoded) + 1
	}
	if len(batch) == 0 {
		return nil
	}
	return c.sendBatch(ctx, batch)
}

func (c *NRClient) sendBatch(ctx context.Context, batch []json.RawMessage) error {
	headers := map[string]string{"Api-Key": c.apiKey}

	resp, respBody, err := c.DoRequestWithRetry(ctx, maxRetries, http.MethodPost, c.url, batch, headers)
	if err != nil {
		return fmt.Errorf("posting new relic event: %w", err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("new relic event api returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

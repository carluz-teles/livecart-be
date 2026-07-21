package events

import (
	"context"
	"encoding/json"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// RegisterHandlers wires every canonical event to its consumer. As flows
// migrate (Fases 04-11) their handlers are registered here. Fase 01 ships only
// the smoke-test handler.
func RegisterHandlers(mux *asynq.ServeMux, log *zap.Logger) {
	mux.HandleFunc(string(TestPing), handleTestPing(log))

	// Groups A/C — event/session/cart lifecycle. Observability-only for now;
	// richer consumers (notifications, analytics) arrive in later phases. Every
	// emitted event needs a handler or asynq reports "handler not found".
	mux.HandleFunc(string(EventEventCreated), logEvent(log))
	mux.HandleFunc(string(EventEventEnded), logEvent(log))
	mux.HandleFunc(string(SessionCreated), logEvent(log))
	mux.HandleFunc(string(SessionEnded), logEvent(log))
	mux.HandleFunc(string(CartCreated), logEvent(log))
	mux.HandleFunc(string(CartItemAdded), logEvent(log))
	mux.HandleFunc(string(CartReopened), logEvent(log))
	mux.HandleFunc(string(CartCancelled), logEvent(log))
	mux.HandleFunc(string(CartExpired), logEvent(log))
}

// logEvent is a generic observability consumer: it records that a canonical
// event was delivered, correlated by trace_id via logger.From.
func logEvent(log *zap.Logger) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var env Envelope
		if err := json.Unmarshal(t.Payload(), &env); err != nil {
			return asynq.SkipRetry
		}
		logger.From(ctx, log).Info("event observed",
			zap.String("event", string(env.Name)),
			zap.String("event_id", env.EventID),
			zap.String("source", string(env.Source)),
		)
		return nil
	}
}

// handleTestPing is a no-op consumer that proves the pipeline end to end:
// publish -> Redis -> worker -> handler. It logs via logger.From so store and
// trace context (once Fase 02 lands) show up automatically.
func handleTestPing(log *zap.Logger) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var env Envelope
		if err := json.Unmarshal(t.Payload(), &env); err != nil {
			// Non-retryable: a malformed payload will never parse.
			return asynq.SkipRetry
		}
		logger.From(ctx, log).Info("test.ping received",
			zap.String("event_id", env.EventID),
			zap.String("source", string(env.Source)),
		)
		return nil
	}
}

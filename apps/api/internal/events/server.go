package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

var consumerTracer = otel.Tracer("livecart/events")

// Server consumes canonical events from Redis and dispatches them to registered
// handlers. It mirrors the Start()/Stop() lifecycle of the existing background
// workers (see internal/integration/expiry_worker.go) so main.go can manage it
// uniformly and shut it down gracefully.
type Server struct {
	inner  *asynq.Server
	mux    *asynq.ServeMux
	logger *zap.Logger
}

// ServerConfig configures the event server.
type ServerConfig struct {
	RedisAddr   string
	Logger      *zap.Logger
	Concurrency int // total worker goroutines; default 10
}

// NewServer builds the asynq server with the LiveCart queue taxonomy and wires
// the handler registry.
func NewServer(cfg ServerConfig) *Server {
	concurrency := cfg.Concurrency
	if concurrency == 0 {
		concurrency = 10
	}
	log := cfg.Logger.Named("events-server")

	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.RedisAddr},
		asynq.Config{
			Concurrency: concurrency,
			// Relative queue priorities (weighted, not worker counts).
			Queues: map[string]int{
				QueueFastTrack: 4,
				QueueNormal:    2,
				QueueBatch:     1,
			},
			Logger:       &zapAsynqLogger{sugar: log.Sugar()},
			ErrorHandler: asynq.ErrorHandlerFunc(errorHandler(log)),
		},
	)

	mux := asynq.NewServeMux()
	// Continue the producer's trace and expose trace_id to logger.From for every
	// event before it reaches its handler.
	mux.Use(traceMiddleware)
	RegisterHandlers(mux, log)

	return &Server{inner: srv, mux: mux, logger: log}
}

// traceMiddleware reconstructs the producer's span context from the envelope
// and starts a consumer span, making trace_id available to logger.From inside
// the handler so consumer logs correlate with the originating request trace.
func traceMiddleware(next asynq.Handler) asynq.Handler {
	return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
		var env Envelope
		_ = json.Unmarshal(t.Payload(), &env)

		if env.TraceID != "" && env.SpanID != "" {
			if tid, err := trace.TraceIDFromHex(env.TraceID); err == nil {
				if sid, err := trace.SpanIDFromHex(env.SpanID); err == nil {
					sc := trace.NewSpanContext(trace.SpanContextConfig{
						TraceID:    tid,
						SpanID:     sid,
						TraceFlags: trace.FlagsSampled,
						Remote:     true,
					})
					ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
				}
			}
		}

		ctx, span := consumerTracer.Start(ctx, "event "+t.Type(), trace.WithSpanKind(trace.SpanKindConsumer))
		defer span.End()

		if sc := span.SpanContext(); sc.HasTraceID() {
			//nolint:staticcheck // string key matches logger.From / c.Locals convention
			ctx = context.WithValue(ctx, logger.TraceIDKey, sc.TraceID().String())
		}
		return next.ProcessTask(ctx, t)
	})
}

// Start begins processing tasks in the background (non-blocking).
func (s *Server) Start() error {
	if err := s.inner.Start(s.mux); err != nil {
		return fmt.Errorf("starting event server: %w", err)
	}
	s.logger.Info("event server started")
	return nil
}

// Stop gracefully drains in-flight tasks and stops the server. Named Stop (not
// Shutdown) to match the worker lifecycle managed in main.go.
func (s *Server) Stop() {
	s.inner.Shutdown()
	s.logger.Info("event server stopped")
}

// errorHandler logs a task that exhausted its handling attempt.
func errorHandler(log *zap.Logger) func(context.Context, *asynq.Task, error) {
	return func(ctx context.Context, task *asynq.Task, err error) {
		retried, _ := asynq.GetRetryCount(ctx)
		maxRetry, _ := asynq.GetMaxRetry(ctx)
		log.Error("event handler failed",
			zap.String("event", task.Type()),
			zap.Int("retried", retried),
			zap.Int("max_retry", maxRetry),
			zap.Error(err),
		)
	}
}

// zapAsynqLogger adapts *zap.SugaredLogger to the asynq.Logger interface.
type zapAsynqLogger struct{ sugar *zap.SugaredLogger }

func (l *zapAsynqLogger) Debug(args ...interface{}) { l.sugar.Debug(args...) }
func (l *zapAsynqLogger) Info(args ...interface{})  { l.sugar.Info(args...) }
func (l *zapAsynqLogger) Warn(args ...interface{})  { l.sugar.Warn(args...) }
func (l *zapAsynqLogger) Error(args ...interface{}) { l.sugar.Error(args...) }
func (l *zapAsynqLogger) Fatal(args ...interface{}) { l.sugar.Fatal(args...) }

package telemetry

import (
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"livecart/apps/api/lib/logger"
)

var tracer = otel.Tracer("livecart/http")

// Middleware opens a server span per request, continuing any inbound W3C trace
// context. It stores the trace id in c.Locals (so the access logger and error
// logger emit it) and swaps the request context via c.SetUserContext so
// downstream services and event publishers inherit the span.
func Middleware() fiber.Handler {
	propagator := otel.GetTextMapPropagator()
	return func(c *fiber.Ctx) error {
		ctx := propagator.Extract(c.UserContext(), &fiberHeaderCarrier{c: c})

		ctx, span := tracer.Start(ctx, c.Method()+" "+c.Path(),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", c.Method()),
				attribute.String("http.route", c.Path()),
			),
		)
		defer span.End()

		if sc := span.SpanContext(); sc.HasTraceID() {
			c.Locals(logger.TraceIDKey, sc.TraceID().String())
		}
		c.SetUserContext(ctx)

		err := c.Next()

		span.SetAttributes(attribute.Int("http.status_code", c.Response().StatusCode()))
		if err != nil {
			span.RecordError(err)
		}
		return err
	}
}

// fiberHeaderCarrier adapts a Fiber request's headers to the OTEL
// TextMapCarrier interface for trace-context extraction.
type fiberHeaderCarrier struct{ c *fiber.Ctx }

var _ propagation.TextMapCarrier = (*fiberHeaderCarrier)(nil)

func (f *fiberHeaderCarrier) Get(key string) string { return f.c.Get(key) }

func (f *fiberHeaderCarrier) Set(key, value string) { f.c.Set(key, value) }

func (f *fiberHeaderCarrier) Keys() []string {
	keys := make([]string, 0)
	f.c.Request().Header.VisitAll(func(k, _ []byte) {
		keys = append(keys, string(k))
	})
	return keys
}

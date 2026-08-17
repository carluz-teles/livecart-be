package telemetry

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
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
		if liveEventID := resolveLiveEventID(c); liveEventID != "" {
			span.SetAttributes(attribute.String("live_event_id", liveEventID))
			c.Locals(logger.LiveEventIDKey, liveEventID)
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

// resolveLiveEventID extracts the live_events.id from the raw request path
// for routes scoped to a live event: /events/:id/... and its /lives/:id/...
// alias (internal/live/handler.go RegisterRoutes), plus
// /events/:eventId/coupons (internal/coupon/handler.go). It parses c.Path()
// directly instead of c.Route()/c.Params() because this middleware runs
// BEFORE Fiber resolves the leaf route (c.Next() hasn't executed yet), so the
// route's own path params are not populated at this point — see Fiber's
// router.next(), which sets c.route/c.values only as each stack entry is
// matched during the c.Next() walk.
//
// The segment right after "/events" or "/lives" is only treated as a
// live_event_id when it parses as a UUID (live_events.id's column type). This
// is a robust allowlist: any static sub-route — current ("/", "/stats",
// "/posts") or future — is a non-UUID literal and is therefore ignored
// without needing to keep a blocklist in sync with internal/live/handler.go.
func resolveLiveEventID(c *fiber.Ctx) string {
	segments := strings.Split(c.Path(), "/")
	for i, seg := range segments {
		if seg != "events" && seg != "lives" {
			continue
		}
		if i+1 >= len(segments) {
			return ""
		}
		candidate := segments[i+1]
		if _, err := uuid.Parse(candidate); err != nil {
			return ""
		}
		return candidate
	}
	return ""
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

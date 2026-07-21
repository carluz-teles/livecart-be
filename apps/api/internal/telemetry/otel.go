// Package telemetry wires OpenTelemetry tracing into the app and integrates it
// with the existing zap logger (every log carries trace_id) and the async
// event pipeline (trace context propagates producer -> consumer).
//
// When no OTLP endpoint is configured (local dev without a collector, tests)
// Init installs only the W3C propagator and leaves the global no-op tracer in
// place, so nothing is exported and nothing breaks.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Config configures tracing setup.
type Config struct {
	// Endpoint is the OTLP gRPC endpoint (host:port). Empty disables exporting
	// (Jaeger locally via OTEL_EXPORTER_OTLP_ENDPOINT, Datadog agent in staging/prod).
	Endpoint    string
	ServiceName string
	Environment string
}

// Init sets the global W3C propagator and, when an endpoint is configured, a
// TracerProvider exporting over OTLP/gRPC. The returned shutdown func is always
// non-nil and safe to defer.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// Always propagate W3C trace context + baggage so trace headers flow even
	// when this service does not export (e.g. behind a gateway that does).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		// No collector configured: keep the default no-op tracer.
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTEL resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

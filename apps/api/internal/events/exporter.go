package events

import "context"

// Exporter forwards select canonical facts to an external telemetry pipeline
// (New Relic custom events — telemetry dashboard project, Fatia 2). Dispatch
// never returns an error: a telemetry-pipeline outage (network, 4xx/5xx) must
// never break the outbox/asynq consumer pipeline, so implementations own their
// own failure handling (structured logging) internally.
//
// Declared here — not in internal/telemetry/exporter — so this package never
// imports the concrete implementation (it would create an import cycle, since
// the exporter needs events.Envelope). The composition root (main.newApp)
// wires the concrete *exporter.Listeners in, structurally satisfying this
// interface.
type Exporter interface {
	Dispatch(ctx context.Context, env Envelope)
}

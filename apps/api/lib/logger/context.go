package logger

import (
	"context"

	"go.uber.org/zap"
)

// Chaves lidas do contexto por From. Nos handlers HTTP elas chegam de graça:
// c.Locals(k, v) grava no UserValue do fasthttp e c.Context().Value(k) lê o
// mesmo valor — então o que os middlewares setam em Locals já é visível aqui.
// Workers e webhooks sem request usam WithStore para popular as mesmas chaves.
const (
	StoreIDKey   = "store_id"
	StoreSlugKey = "store_slug"
	// TraceIDKey carries the OTEL trace id. The telemetry middleware sets it in
	// c.Locals per request so every log correlates with its distributed trace.
	TraceIDKey = "trace_id"
	// LiveEventIDKey carries the live_events.id (the "live event"/campaign
	// domain entity — NOT the outbox Envelope.EventID, which identifies an
	// async event instance for idempotency). Set by WithLiveEvent for
	// workers/consumers, and by c.Locals in the telemetry middleware for
	// requests scoped to a live event.
	LiveEventIDKey = "live_event_id"
)

// setUserValuer é o RequestCtx do fasthttp. Quando o ctx é o próprio request
// (rotas públicas que resolvem o store tardiamente, ex.: checkout por token),
// gravar via SetUserValue equivale a setar c.Locals — o access log e os logs
// centrais de erro do httpx passam a enxergar o store também.
type setUserValuer interface {
	SetUserValue(key, value any)
}

// WithStore devolve um contexto que carrega o store para os logs. Uso em
// workers/webhooks/services que resolvem o store fora do middleware HTTP.
func WithStore(ctx context.Context, storeID, storeSlug string) context.Context {
	if uv, ok := ctx.(setUserValuer); ok {
		if storeID != "" {
			uv.SetUserValue(StoreIDKey, storeID)
		}
		if storeSlug != "" {
			uv.SetUserValue(StoreSlugKey, storeSlug)
		}
		return ctx // Value() do fasthttp já lê os UserValues
	}
	ctx = context.WithValue(ctx, StoreIDKey, storeID) //nolint:staticcheck // chave string casa com c.Locals
	if storeSlug != "" {
		ctx = context.WithValue(ctx, StoreSlugKey, storeSlug) //nolint:staticcheck
	}
	return ctx
}

// WithLiveEvent devolve um contexto que carrega o live event para os logs.
// Mesmo padrão de WithStore: workers/consumers/webhooks que resolvem o live
// event fora do middleware HTTP chamam isto para popular a mesma chave que
// From lê.
func WithLiveEvent(ctx context.Context, liveEventID string) context.Context {
	if uv, ok := ctx.(setUserValuer); ok {
		if liveEventID != "" {
			uv.SetUserValue(LiveEventIDKey, liveEventID)
		}
		return ctx // Value() do fasthttp já lê os UserValues
	}
	ctx = context.WithValue(ctx, LiveEventIDKey, liveEventID) //nolint:staticcheck // chave string casa com c.Locals
	return ctx
}

// From devolve o logger enriquecido com store_id e store_slug quando o
// contexto os carrega; caso contrário devolve o base intocado. Nunca retorna
// nil e aceita ctx nil, então é seguro em qualquer call site.
func From(ctx context.Context, base *zap.Logger) *zap.Logger {
	if ctx == nil {
		return base
	}
	fields := make([]zap.Field, 0, 4)
	if id, ok := ctx.Value(StoreIDKey).(string); ok && id != "" {
		fields = append(fields, zap.String("store_id", id))
	}
	if slug, ok := ctx.Value(StoreSlugKey).(string); ok && slug != "" {
		fields = append(fields, zap.String("store_slug", slug))
	}
	if traceID, ok := ctx.Value(TraceIDKey).(string); ok && traceID != "" {
		fields = append(fields, zap.String("trace_id", traceID))
	}
	if liveEventID, ok := ctx.Value(LiveEventIDKey).(string); ok && liveEventID != "" {
		fields = append(fields, zap.String("live_event_id", liveEventID))
	}
	if len(fields) == 0 {
		return base
	}
	return base.With(fields...)
}

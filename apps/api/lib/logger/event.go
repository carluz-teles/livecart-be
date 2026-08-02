package logger

import (
	"context"

	"go.uber.org/zap"
)

// Category tags for the structured-log convention. They are DUPLICATED here as
// plain strings — NOT imported from httpx — on purpose: logger is a leaf package
// (it imports only zap + config), and importing httpx would create a cycle
// (httpx already depends transitively on the same wiring). They MUST stay
// string-equal to the httpx.Category consts; a guard test in package httpx
// (which MAY import logger) asserts the equality and fails the moment they drift.
const (
	CatDomain         = "DOMAIN"
	CatValidation     = "VALIDATION"
	CatRepository     = "REPOSITORY"
	CatInfrastructure = "INFRASTRUCTURE"
)

// EventBuilder is a fluent structured-log builder layered OVER From(ctx, base).
// It stamps a ::func:: marker, an optional category tag and an ordered metadata
// bag onto a single line. It is additive: the ~578 existing From call sites are
// untouched and adoption is opportunistic. Construct it via Event().
type EventBuilder struct {
	log    *zap.Logger
	fn     string
	cat    string
	fields []zap.Field
}

// Event opens a log event bound to ctx. From(ctx, base) runs once here, so the
// resulting line inherits store_id/store_slug/trace_id when the context carries
// them (a nil ctx is safe → the base logger, no store fields). fn is a MANUAL,
// greppable logical name emitted as fn="::fn::" — a stable, refactor-proof label
// distinct from zap's physical caller (file:line).
//
//	logger.Event(ctx, base, "createCoupon").
//	    Cat(logger.CatDomain).
//	    Meta("cart_id", cart.ID).Meta("code", code).
//	    Warn("coupon already redeemed")
func Event(ctx context.Context, base *zap.Logger, fn string) *EventBuilder {
	return &EventBuilder{log: From(ctx, base), fn: "::" + fn + "::"}
}

// Cat tags the event with an error category (use the Cat* consts). Without a Cat
// call the line carries no category key at all — not an empty string.
func (e *EventBuilder) Cat(c string) *EventBuilder {
	e.cat = c
	return e
}

// Meta appends one key/value to the metadata bag. Repeated calls accumulate in
// call order; zap.Any keeps it ergonomic for any value.
func (e *EventBuilder) Meta(k string, v any) *EventBuilder {
	e.fields = append(e.fields, zap.Any(k, v))
	return e
}

// fieldset assembles the final slice: fn first, category next (only when set),
// then the metadata bag in insertion order. Pre-sized to avoid a re-grow.
func (e *EventBuilder) fieldset() []zap.Field {
	out := make([]zap.Field, 0, len(e.fields)+2)
	out = append(out, zap.String("fn", e.fn))
	if e.cat != "" {
		out = append(out, zap.String("category", e.cat))
	}
	return append(out, e.fields...)
}

// Info/Warn/Error are the terminals. Only these three levels are exposed — the
// convention is for domain/observability lines, not Debug/DPanic/Fatal.
func (e *EventBuilder) Info(msg string)  { e.log.Info(msg, e.fieldset()...) }
func (e *EventBuilder) Warn(msg string)  { e.log.Warn(msg, e.fieldset()...) }
func (e *EventBuilder) Error(msg string) { e.log.Error(msg, e.fieldset()...) }

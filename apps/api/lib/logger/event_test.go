package logger

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// newObserved wires a zap logger to an in-memory observer so tests assert on the
// exact fields emitted (same molde as httpx/errors_test.go: observer.New + a
// real *zap.Logger).
func newObserved() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.InfoLevel)
	return zap.New(core), logs
}

// TestEvent_FnMarker — AC 3: the manual ::func:: marker is emitted verbatim as
// fn=="::createCoupon::" (greppable logical name, not zap's physical caller).
func TestEvent_FnMarker(t *testing.T) {
	base, logs := newObserved()

	Event(context.Background(), base, "createCoupon").Info("x")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log line, got %d", len(entries))
	}
	if m := entries[0].ContextMap(); m["fn"] != "::createCoupon::" {
		t.Fatalf("fn: want ::createCoupon::, got %v", m["fn"])
	}
}

// TestEvent_Category — AC 4: Cat() adds category; without Cat the category key is
// ABSENT (not an empty string), so unclassified events don't pollute the field.
func TestEvent_Category(t *testing.T) {
	t.Run("present when Cat called", func(t *testing.T) {
		base, logs := newObserved()
		Event(context.Background(), base, "fn").Cat(CatDomain).Info("x")
		if m := logs.All()[0].ContextMap(); m["category"] != "DOMAIN" {
			t.Fatalf("category: want DOMAIN, got %v", m["category"])
		}
	})
	t.Run("absent when Cat omitted", func(t *testing.T) {
		base, logs := newObserved()
		Event(context.Background(), base, "fn").Info("x")
		if m := logs.All()[0].ContextMap(); func() bool { _, ok := m["category"]; return ok }() {
			t.Fatalf("category key must be absent when Cat not called")
		}
	})
}

// TestEvent_MetaBag — AC 5: repeated Meta() calls accumulate, keep their values,
// and land in call order after fn.
func TestEvent_MetaBag(t *testing.T) {
	base, logs := newObserved()

	Event(context.Background(), base, "fn").
		Meta("cart_id", "c_9").
		Meta("code", "X").
		Warn("boom")

	entry := logs.All()[0]
	m := entry.ContextMap()
	if m["cart_id"] != "c_9" {
		t.Fatalf("cart_id: want c_9, got %v", m["cart_id"])
	}
	if m["code"] != "X" {
		t.Fatalf("code: want X, got %v", m["code"])
	}

	// fn first, then the two Meta fields in insertion order.
	want := []string{"fn", "cart_id", "code"}
	for i, w := range want {
		if entry.Context[i].Key != w {
			t.Fatalf("field order: want %v, got key[%d]=%q", want, i, entry.Context[i].Key)
		}
	}
}

// TestEvent_ComposesWithFrom — AC 6: Event layers OVER From(ctx), so a line carries
// store_id/store_slug/trace_id from the context PLUS fn + category + Meta.
func TestEvent_ComposesWithFrom(t *testing.T) {
	base, logs := newObserved()

	ctx := WithStore(context.Background(), "st_123", "acme")
	ctx = context.WithValue(ctx, TraceIDKey, "trace_abc") //nolint:staticcheck // key casa com c.Locals

	Event(ctx, base, "checkout").
		Cat(CatInfrastructure).
		Meta("cart_id", "c_9").
		Warn("x")

	m := logs.All()[0].ContextMap()
	for k, want := range map[string]string{
		"store_id":   "st_123",
		"store_slug": "acme",
		"trace_id":   "trace_abc",
		"fn":         "::checkout::",
		"category":   "INFRASTRUCTURE",
		"cart_id":    "c_9",
	} {
		if m[k] != want {
			t.Fatalf("%s: want %q, got %v", k, want, m[k])
		}
	}
}

// TestEvent_NilContextSafe — AC 6 (nil ctx): a nil context must not panic; the line
// carries fn but no store fields.
func TestEvent_NilContextSafe(t *testing.T) {
	base, logs := newObserved()

	Event(nil, base, "worker").Info("x") //nolint:staticcheck // explicitly testing nil ctx

	m := logs.All()[0].ContextMap()
	if m["fn"] != "::worker::" {
		t.Fatalf("fn: want ::worker::, got %v", m["fn"])
	}
	if _, ok := m["store_id"]; ok {
		t.Fatalf("store_id must be absent for nil ctx, got %v", m["store_id"])
	}
}

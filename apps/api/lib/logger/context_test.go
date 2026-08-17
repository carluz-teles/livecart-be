package logger

import (
	"context"
	"testing"
)

// TestWithLiveEvent_From — mirrors WithStore's own contract: WithLiveEvent
// populates the context key From reads, so a log emitted downstream carries
// live_event_id. Table-driven against the same behaviors WithStore already
// has to prove the two helpers share one pattern.
func TestWithLiveEvent_From(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		liveEventID string
		wantField   bool
	}{
		{
			name:        "non-empty live event id is readable via From",
			liveEventID: "22222222-2222-2222-2222-222222222222",
			wantField:   true,
		},
		{
			name:        "empty live event id adds no field",
			liveEventID: "",
			wantField:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base, logs := newObserved()

			ctx := WithLiveEvent(context.Background(), tt.liveEventID)
			From(ctx, base).Info("x")

			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("want 1 log line, got %d", len(entries))
			}
			m := entries[0].ContextMap()
			got, present := m["live_event_id"]
			if present != tt.wantField {
				t.Fatalf("live_event_id present = %v, want %v (value %v)", present, tt.wantField, got)
			}
			if tt.wantField && got != tt.liveEventID {
				t.Fatalf("live_event_id = %v, want %v", got, tt.liveEventID)
			}
		})
	}
}

// TestFrom_CombinesStoreAndLiveEvent — the two ctx helpers are independent:
// setting both store and live event populates both fields on the same logger,
// matching how a consumer handler (server.go's traceMiddleware + a reactor
// calling logger.WithStore) actually stacks them.
func TestFrom_CombinesStoreAndLiveEvent(t *testing.T) {
	t.Parallel()
	base, logs := newObserved()

	ctx := WithStore(context.Background(), "store-1", "loja-1")
	ctx = WithLiveEvent(ctx, "event-1")
	From(ctx, base).Info("x")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log line, got %d", len(entries))
	}
	m := entries[0].ContextMap()
	if m["store_id"] != "store-1" {
		t.Fatalf("store_id = %v, want store-1", m["store_id"])
	}
	if m["store_slug"] != "loja-1" {
		t.Fatalf("store_slug = %v, want loja-1", m["store_slug"])
	}
	if m["live_event_id"] != "event-1" {
		t.Fatalf("live_event_id = %v, want event-1", m["live_event_id"])
	}
}

// TestFrom_NilContext — From must never panic and must return the base logger
// untouched when ctx is nil, the same safety net WithStore's caller relies on.
func TestFrom_NilContext(t *testing.T) {
	t.Parallel()
	base, logs := newObserved()

	//nolint:staticcheck // intentional: From must be nil-ctx-safe, this proves it
	From(nil, base).Info("x")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log line, got %d", len(entries))
	}
	if m := entries[0].ContextMap(); len(m) != 0 {
		t.Fatalf("want no extra fields for a nil ctx, got %v", m)
	}
}

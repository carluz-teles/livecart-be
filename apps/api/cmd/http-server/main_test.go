package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/telemetry/exporter"
)

// slowSender is a fake eventSender (see exporter.Listeners) whose SendEvent
// blocks for longer than telemetryDispatchTimeout, simulating a New
// Relic outage. It structurally satisfies exporter's unexported eventSender
// interface (Go interface satisfaction is structural, not name-based).
type slowSender struct {
	delay time.Duration

	mu    sync.Mutex
	calls int
}

func (s *slowSender) SendEvent(ctx context.Context, _ any) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return ctx.Err()
}

func (s *slowSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestDispatchTelemetryAsync_DoesNotBlockCaller proves the fix for the
// reviewer's HIGH finding: cart.paid/event.ended must not let a slow/
// unavailable New Relic burn the asynq task's 15s budget before the
// business-critical fan-out even starts. The fake sender sleeps well past
// telemetryDispatchTimeout; dispatchTelemetryAsync must still return almost
// immediately because Dispatch itself runs in a detached goroutine.
func TestDispatchTelemetryAsync_DoesNotBlockCaller(t *testing.T) {
	sender := &slowSender{delay: telemetryDispatchTimeout + 2*time.Second}
	listeners := exporter.NewListeners(sender, exporter.Config{Enabled: true}, zap.NewNop())

	env := events.Envelope{
		Name:    events.CartPaid,
		EventID: "evt-1",
		Payload: []byte(`{"cart_id":"c1","store_id":"s1","payment_id":"p1","payment_method":"credit_card","gmv_cents":1000}`),
	}

	// The caller's ctx has a generous deadline (mirrors the asynq task's
	// QueueNormal: 15s budget) so a pre-fix synchronous Dispatch would not be
	// cut off by ctx itself — only the fix (detached goroutine) prevents the
	// block.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	dispatchTelemetryAsync(ctx, listeners, env, zap.NewNop())
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("dispatchTelemetryAsync blocked the caller for %s, want near-instant return", elapsed)
	}

	// Give the detached goroutine time to actually run and hit its own
	// timeout, proving it eventually gives up instead of leaking forever.
	deadline := time.Now().Add(telemetryDispatchTimeout + time.Second)
	for sender.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if sender.callCount() == 0 {
		t.Fatal("expected the detached goroutine to eventually call SendEvent")
	}
}

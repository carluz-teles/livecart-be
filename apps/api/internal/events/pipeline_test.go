package events

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// redisAddr returns the address to test against, skipping the test when no
// Redis is reachable (e.g. plain `go test ./...` without infra).
func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		t.Skipf("redis not reachable at %s: %v", addr, err)
	}
	_ = conn.Close()
	return addr
}

// TestPipeline_EnqueueConsume proves the round trip: a canonical event
// published via the Client is delivered to an asynq consumer over Redis, with
// the envelope (name, source, metadata) intact.
func TestPipeline_EnqueueConsume(t *testing.T) {
	addr := redisAddr(t)

	client := NewClient(addr)
	defer client.Close()

	got := make(chan Envelope, 1)
	srv := asynq.NewServer(asynq.RedisClientOpt{Addr: addr}, asynq.Config{
		Concurrency: 1,
		Queues:      map[string]int{QueueNormal: 1},
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(string(TestPing), func(ctx context.Context, task *asynq.Task) error {
		var env Envelope
		if err := json.Unmarshal(task.Payload(), &env); err != nil {
			return err
		}
		got <- env
		return nil
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer srv.Shutdown()

	want := Envelope{
		EventID:    uuid.NewString(),
		Name:       TestPing,
		Source:     SourceInternal,
		Metadata:   map[string]string{"origin": "unit-test"},
		OccurredAt: time.Now().UTC(),
	}
	if _, err := client.Enqueue(context.Background(), want); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case env := <-got:
		if env.EventID != want.EventID || env.Name != want.Name || env.Source != want.Source {
			t.Fatalf("envelope mismatch: got %+v want %+v", env, want)
		}
		if env.Metadata["origin"] != "unit-test" {
			t.Fatalf("metadata not propagated: %+v", env.Metadata)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event was not consumed within 5s")
	}
}

// TestPipeline_TracePropagation proves the producer's trace context is injected
// into the envelope so the consumer can continue the same distributed trace.
func TestPipeline_TracePropagation(t *testing.T) {
	addr := redisAddr(t)

	// A real SDK provider generates valid trace/span ids (no exporter needed).
	otel.SetTracerProvider(sdktrace.NewTracerProvider())

	client := NewClient(addr)
	defer client.Close()

	got := make(chan Envelope, 1)
	srv := asynq.NewServer(asynq.RedisClientOpt{Addr: addr}, asynq.Config{
		Concurrency: 1,
		Queues:      map[string]int{QueueNormal: 1},
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(string(TestPing), func(ctx context.Context, task *asynq.Task) error {
		var env Envelope
		if err := json.Unmarshal(task.Payload(), &env); err != nil {
			return err
		}
		got <- env
		return nil
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer srv.Shutdown()

	ctx, span := otel.Tracer("test").Start(context.Background(), "producer")
	wantTraceID := span.SpanContext().TraceID().String()
	if _, err := client.Enqueue(ctx, Envelope{EventID: uuid.NewString(), Name: TestPing, Source: SourceInternal}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	span.End()

	select {
	case env := <-got:
		if env.TraceID != wantTraceID {
			t.Fatalf("trace id not propagated: got %q want %q", env.TraceID, wantTraceID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event was not consumed within 5s")
	}
}

package events

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
)

// TestOutbox_RelayDelivers proves the Fase 03 path: an event emitted into the
// outbox (in a tx) is drained by the relay to Redis, consumed, and the outbox
// row ends up marked published. Requires DATABASE_URL + REDIS_ADDR with the
// 000089 migration applied.
func TestOutbox_RelayDelivers(t *testing.T) {
	addr := redisAddr(t)
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("no DATABASE_URL")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting db: %v", err)
	}
	defer pool.Close()

	// Isolate this run.
	if _, err := pool.Exec(ctx, "DELETE FROM event_outbox"); err != nil {
		t.Fatalf("cleanup outbox: %v", err)
	}
	_, _ = pool.Exec(ctx, "DELETE FROM event_consumed")

	// Emit into the outbox in a transaction (as a domain service would).
	eventID := uuid.NewString()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Emit(ctx, sqlc.New(tx), Envelope{EventID: eventID, Name: TestPing, Source: SourceInternal, DedupKey: "k1"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Consumer.
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
		t.Fatalf("start server: %v", err)
	}
	defer srv.Shutdown()

	// Relay drains the outbox to Redis.
	client := NewClient(asynq.RedisClientOpt{Addr: addr}, zap.NewNop())
	defer client.Close()
	relay := NewRelay(RelayConfig{Pool: pool, Client: client, Logger: zap.NewNop(), Interval: 200 * time.Millisecond})
	relay.Start()
	defer relay.Stop()

	select {
	case env := <-got:
		if env.EventID != eventID {
			t.Fatalf("wrong event delivered: got %q want %q", env.EventID, eventID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered within 5s")
	}

	// The outbox row must be marked published.
	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM event_outbox WHERE event_id = $1", eventID).Scan(&status); err != nil {
		t.Fatalf("reading outbox status: %v", err)
	}
	if status != "published" {
		t.Fatalf("outbox row not published: status=%q", status)
	}
}

//go:build integration

package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDeferredTaskExhaustionRemainsAnErrorWithTaskID(t *testing.T) {
	addr := redisAddr(t)
	queue := "audit-" + uuid.NewString()
	core, logs := observer.New(zap.DebugLevel)
	logged := make(chan struct{}, 1)
	server := asynq.NewServer(asynq.RedisClientOpt{Addr: addr, DB: 15}, asynq.Config{
		Concurrency: 1, Queues: map[string]int{queue: 1},
		ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
			errorHandler(zap.New(core))(ctx, task, err)
			logged <- struct{}{}
		}),
	})
	if err := server.Start(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return NewDeferredError("busy") })); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	client := asynq.NewClient(asynq.RedisClientOpt{Addr: addr, DB: 15})
	defer client.Close()
	payload, err := json.Marshal(Envelope{EventID: uuid.NewString(), Metadata: map[string]string{"store_id": "store-1"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Enqueue(asynq.NewTask("audit", payload), asynq.Queue(queue), asynq.MaxRetry(0))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-logged:
	case <-time.After(10 * time.Second):
		t.Fatal("task was not handled")
	}
	entries := logs.FilterMessage("event handler failed — DEAD-LETTERED (archived)").All()
	if len(entries) != 1 || entries[0].Level != zap.ErrorLevel {
		t.Fatalf("terminal error hidden: %+v", logs.All())
	}
	fields := entries[0].ContextMap()
	if fields["task_id"] != info.ID || fields["queue"] != queue || fields["store_id"] != "store-1" {
		t.Fatalf("missing queue correlation: %v", fields)
	}
}

//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
)

func TestCartExpiryRearmsWhilePreviousTaskIsActive(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set: isolated Redis is required")
	}
	for _, legacy := range []bool{false, true} {
		name := "current task"
		if legacy {
			name = "legacy task"
		}
		t.Run(name, func(t *testing.T) {
			opt := asynq.RedisClientOpt{Addr: addr, DB: 14}
			client := events.NewClient(opt, zap.NewNop())
			defer client.Close()
			inspector := asynq.NewInspector(opt)
			defer inspector.Close()
			cart := uuid.NewString()
			oldDeadline := time.Now().Add(-time.Minute)
			deadline := time.Now().Add(time.Hour).Truncate(time.Second)
			oldID := cartExpireTaskID(cart, oldDeadline)
			if legacy {
				oldID = "cart-expire:" + cart
			}
			newID := cartExpireTaskID(cart, deadline)
			defer inspector.DeleteTask(events.QueueNormal, newID)
			scheduler := cartExpiryScheduler{client: client}
			ready := make(chan error, 1)
			finish := make(chan struct{})
			srv := asynq.NewServer(opt, asynq.Config{Concurrency: 1,
				Queues: map[string]int{events.QueueNormal: 1}, TaskCheckInterval: 10 * time.Millisecond})
			mux := asynq.NewServeMux()
			mux.HandleFunc(string(events.CartExpire), func(ctx context.Context, _ *asynq.Task) error {
				err := scheduler.RescheduleCartExpiry(ctx, cart, deadline)
				if err == nil {
					err = scheduler.RescheduleCartExpiry(ctx, cart, deadline)
				}
				ready <- err
				select {
				case <-finish:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err := srv.Start(mux); err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown()
			defer close(finish)
			if err := client.Schedule(t.Context(), oldDeadline, events.CartExpire, oldID,
				struct {
					CartID string `json:"cart_id"`
				}{cart}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("expiry worker did not rearm")
			}
			old, err := inspector.GetTaskInfo(events.QueueNormal, oldID)
			if err != nil || old.State != asynq.TaskStateActive {
				t.Fatalf("test must inspect while old task is active: %+v %v", old, err)
			}
			next, err := inspector.GetTaskInfo(events.QueueNormal, newID)
			if err != nil || next.State != asynq.TaskStateScheduled || !next.NextProcessAt.Equal(deadline) {
				t.Fatalf("extended deadline was lost: %+v %v", next, err)
			}
			var env events.Envelope
			var payload struct {
				CartID string `json:"cart_id"`
			}
			if err := json.Unmarshal(next.Payload, &env); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.CartID != cart {
				t.Fatalf("wrong purchase scheduled: %+v %v", payload, err)
			}
		})
	}
}

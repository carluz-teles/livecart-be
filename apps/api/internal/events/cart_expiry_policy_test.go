//go:build integration

package events

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

func TestCartExpiryEnqueueUsesCancellationBudgetAndRetries(t *testing.T) {
	client := NewClient(asynq.RedisClientOpt{Addr: redisAddr(t), DB: 15}, zap.NewNop())
	t.Cleanup(func() { _ = client.Close() })
	queue := "expiry-policy-" + uuid.NewString()
	for _, name := range []Name{CartExpired, TestPing} {
		info, err := client.Enqueue(t.Context(), Envelope{
			EventID: uuid.NewString(), Name: name, OccurredAt: time.Now(),
		}, asynq.Queue(queue))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := client.inspector.DeleteTask(queue, info.ID); err != nil {
				t.Error(err)
			}
		})
		wantTimeout, wantRetries := 15*time.Second, 3
		if name == CartExpired {
			wantTimeout, wantRetries = 90*time.Second, 10
		}
		if info.Timeout != wantTimeout || info.MaxRetry != wantRetries {
			t.Fatalf("%s timeout=%s retries=%d, want %s/%d", name, info.Timeout, info.MaxRetry, wantTimeout, wantRetries)
		}
	}
}

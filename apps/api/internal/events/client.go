package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

// Client publishes canonical events onto the asynq queue. It is used by the
// outbox relay (Fase 03), not directly by domain services — services emit via
// the transactional outbox so an event is never lost between the state change
// commit and the enqueue.
type Client struct {
	inner *asynq.Client
}

// NewClient builds an event publisher backed by Redis at redisAddr.
func NewClient(redisAddr string) *Client {
	return &Client{inner: asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddr})}
}

// Enqueue serializes the envelope as an asynq task and enqueues it. The task
// type is the canonical event name; the whole envelope (source, metadata, trace
// context, payload) is the task payload so the consumer can reconstruct it.
//
// The queue and its default retry/timeout policy come from the envelope unless
// the caller overrides them via opts.
func (c *Client) Enqueue(ctx context.Context, env Envelope, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshaling event envelope: %w", err)
	}

	queue := env.Queue()
	baseOpts := []asynq.Option{asynq.Queue(queue)}
	if p, ok := DefaultPolicies[queue]; ok {
		baseOpts = append(baseOpts, asynq.MaxRetry(p.MaxRetry), asynq.Timeout(p.Timeout))
	}
	// Use the event id as the task id so a duplicated relay enqueue is a no-op
	// on the queue itself (belt-and-suspenders with the outbox published flag).
	if env.EventID != "" {
		baseOpts = append(baseOpts, asynq.TaskID(env.EventID))
	}

	task := asynq.NewTask(string(env.Name), data, append(baseOpts, opts...)...)
	info, err := c.inner.EnqueueContext(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("enqueuing event %s: %w", env.Name, err)
	}
	return info, nil
}

// Close releases the underlying Redis connection.
func (c *Client) Close() error {
	return c.inner.Close()
}

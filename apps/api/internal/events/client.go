package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"
)

// Client publishes canonical events onto the asynq queue. It is used by the
// outbox relay (Fase 03), not directly by domain services — services emit via
// the transactional outbox so an event is never lost between the state change
// commit and the enqueue.
type Client struct {
	inner *asynq.Client
}

// NewClient builds an event publisher backed by the given Redis connection.
// Callers pass a RedisConnOpt (not a bare addr) so managed Redis with a
// password/TLS — e.g. Railway — works, not just local no-auth Redis.
func NewClient(redisOpt asynq.RedisConnOpt) *Client {
	return &Client{inner: asynq.NewClient(redisOpt)}
}

// Enqueue serializes the envelope as an asynq task and enqueues it. The task
// type is the canonical event name; the whole envelope (source, metadata, trace
// context, payload) is the task payload so the consumer can reconstruct it.
//
// The queue and its default retry/timeout policy come from the envelope unless
// the caller overrides them via opts.
func (c *Client) Enqueue(ctx context.Context, env Envelope, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	// Inject the producer's trace context so the consumer continues the span.
	if env.TraceID == "" {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			env.TraceID = sc.TraceID().String()
			env.SpanID = sc.SpanID().String()
		}
	}

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

// Schedule enqueues an internal command task to run at processAt. asynq stores
// it in a per-queue scheduled sorted set (scored by the process-at time) and its
// scheduler forwards it to the ready queue when due — precise ETA execution, not
// the lossy Redis key-TTL / keyspace-notification pattern.
//
// taskID makes scheduling idempotent: a duplicate enqueue while a task with the
// same id is still pending returns ErrTaskIDConflict, which we swallow ("already
// armed"). A task that already ran and completed frees its id, so re-arming
// (self-reschedule on window extension) works.
func (c *Client) Schedule(ctx context.Context, processAt time.Time, name Name, taskID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s command payload: %w", name, err)
	}
	env := Envelope{Name: name, Source: SourceInternal, SchemaVersion: 1, Payload: body}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		env.TraceID = sc.TraceID().String()
		env.SpanID = sc.SpanID().String()
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling %s command envelope: %w", name, err)
	}
	_, err = c.inner.EnqueueContext(ctx, asynq.NewTask(string(name), data),
		asynq.ProcessAt(processAt.UTC()),
		asynq.TaskID(taskID),
		asynq.Queue(QueueNormal),
		asynq.MaxRetry(5),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil // already scheduled — the pending task will handle it
	}
	if err != nil {
		return fmt.Errorf("scheduling %s at %s: %w", name, processAt.UTC().Format(time.RFC3339), err)
	}
	return nil
}

// Close releases the underlying Redis connection.
func (c *Client) Close() error {
	return c.inner.Close()
}

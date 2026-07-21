package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/trace"

	"livecart/apps/api/db/sqlc"
)

// Emit writes a canonical event to the transactional outbox. Call it with a
// tx-scoped *sqlc.Queries (queries.WithTx(tx)) so the event is committed in the
// SAME transaction as the state change — the event is never lost between the
// commit and the enqueue. The relay drains the outbox to asynq.
//
// It fills in an EventID and OccurredAt when absent and injects the current
// trace context so the eventual consumer continues the same distributed trace.
func Emit(ctx context.Context, q *sqlc.Queries, env Envelope) error {
	if env.EventID == "" {
		env.EventID = uuid.NewString()
	}
	if env.OccurredAt.IsZero() {
		env.OccurredAt = time.Now().UTC()
	}
	if env.TraceID == "" {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			env.TraceID = sc.TraceID().String()
			env.SpanID = sc.SpanID().String()
		}
	}

	eventID, err := toPgUUID(env.EventID)
	if err != nil {
		return fmt.Errorf("invalid event id %q: %w", env.EventID, err)
	}

	metadata := []byte("{}")
	if len(env.Metadata) > 0 {
		if metadata, err = json.Marshal(env.Metadata); err != nil {
			return fmt.Errorf("marshaling event metadata: %w", err)
		}
	}
	payload := env.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	return q.InsertOutboxEvent(ctx, sqlc.InsertOutboxEventParams{
		EventID:  eventID,
		Name:     string(env.Name),
		Source:   string(env.Source),
		Metadata: metadata,
		Payload:  payload,
		TraceID:  env.TraceID,
		SpanID:   env.SpanID,
		DedupKey: env.DedupKey,
		Queue:    env.Queue(),
	})
}

// envelopeFromRow reconstructs a wire Envelope from a persisted outbox row.
func envelopeFromRow(r sqlc.EventOutbox) Envelope {
	var metadata map[string]string
	if len(r.Metadata) > 0 {
		_ = json.Unmarshal(r.Metadata, &metadata)
	}
	env := Envelope{
		EventID:  fromPgUUID(r.EventID),
		Name:     Name(r.Name),
		Source:   Source(r.Source),
		Metadata: metadata,
		Payload:  json.RawMessage(r.Payload),
		TraceID:  r.TraceID,
		SpanID:   r.SpanID,
		DedupKey: r.DedupKey,
	}
	if r.CreatedAt.Valid {
		env.OccurredAt = r.CreatedAt.Time
	}
	return env
}

func toPgUUID(s string) (pgtype.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func fromPgUUID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

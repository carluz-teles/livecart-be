package events

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
)

// maxOutboxAttempts caps how many times the relay retries enqueuing a poison
// row before dead-lettering it (status='dead'), so one bad event can't be
// re-scanned forever.
const maxOutboxAttempts = 10

// Relay drains the transactional outbox into asynq. It mirrors the Start()/
// Stop() lifecycle of the other background workers. Each sweep runs in a single
// transaction: it locks a batch of pending rows (FOR UPDATE SKIP LOCKED),
// enqueues each to Redis and marks it published, then commits — so a crash
// mid-sweep leaves rows pending for the next sweep, and the asynq task id
// (= event id) makes a re-enqueue a no-op.
type Relay struct {
	pool     *pgxpool.Pool
	client   *Client
	logger   *zap.Logger
	interval time.Duration
	limit    int32
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// RelayConfig configures the outbox relay.
type RelayConfig struct {
	Pool     *pgxpool.Pool
	Client   *Client
	Logger   *zap.Logger
	Interval time.Duration // default 2s — short, to keep added latency low
	Limit    int32         // rows per sweep; default 100
}

func NewRelay(cfg RelayConfig) *Relay {
	interval := cfg.Interval
	if interval == 0 {
		interval = 2 * time.Second
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = 100
	}
	return &Relay{
		pool:     cfg.Pool,
		client:   cfg.Client,
		logger:   cfg.Logger.Named("events-relay"),
		interval: interval,
		limit:    limit,
		stopCh:   make(chan struct{}),
	}
}

func (r *Relay) Start() {
	r.wg.Add(1)
	go r.run()
	r.logger.Info("outbox relay started", zap.Duration("interval", r.interval), zap.Int32("limit", r.limit))
}

func (r *Relay) Stop() {
	close(r.stopCh)
	r.wg.Wait()
	r.logger.Info("outbox relay stopped")
}

func (r *Relay) run() {
	defer r.wg.Done()

	// Drain once on boot to recover events left pending by a previous crash.
	r.sweep()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.sweep()
		case <-r.stopCh:
			return
		}
	}
}

func (r *Relay) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.logger.Error("outbox sweep: begin tx", zap.Error(err))
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	q := sqlc.New(tx)
	rows, err := q.ListPendingOutbox(ctx, r.limit)
	if err != nil {
		r.logger.Error("outbox sweep: list pending", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}

	published := 0
	for _, row := range rows {
		env := envelopeFromRow(row)
		_, err := r.client.Enqueue(ctx, env)
		// A duplicate/conflict means a prior crashed sweep already enqueued this
		// event — treat it as published so we stop re-trying it.
		if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) && !errors.Is(err, asynq.ErrTaskIDConflict) {
			// Dead-letter a poison row after maxOutboxAttempts so it stops being
			// retried every sweep and stays queryable (status='dead', last_error).
			if int(row.Attempts)+1 >= maxOutboxAttempts {
				r.logger.Error("outbox sweep: dead-lettering event after max attempts",
					zap.String("event_id", env.EventID), zap.String("name", string(env.Name)),
					zap.Int32("attempts", row.Attempts+1), zap.Error(err))
				if mErr := q.MarkOutboxDead(ctx, sqlc.MarkOutboxDeadParams{ID: row.ID, LastError: err.Error()}); mErr != nil {
					r.logger.Error("outbox sweep: mark dead", zap.Error(mErr))
				}
				continue
			}
			r.logger.Warn("outbox sweep: enqueue failed",
				zap.String("event_id", env.EventID), zap.String("name", string(env.Name)),
				zap.Int32("attempts", row.Attempts+1), zap.Error(err))
			if mErr := q.MarkOutboxFailed(ctx, sqlc.MarkOutboxFailedParams{ID: row.ID, LastError: err.Error()}); mErr != nil {
				r.logger.Error("outbox sweep: mark failed", zap.Error(mErr))
			}
			continue
		}
		if err := q.MarkOutboxPublished(ctx, row.ID); err != nil {
			r.logger.Error("outbox sweep: mark published", zap.Error(err))
			continue
		}
		// Nasce aqui na fila o rastro de cada evento de domínio. Correlaciona com
		// o "event observed" do consumidor via event_id/trace_id (mede latência
		// enqueue→processamento). trace_id vem do envelope (o ctx do sweep é novo).
		r.logger.Info("event enqueued",
			zap.String("event", string(env.Name)),
			zap.String("event_id", env.EventID),
			zap.String("source", string(env.Source)),
			zap.String("dedup_key", env.DedupKey),
			zap.String("queue", env.Queue()),
			zap.Int("schema_version", env.SchemaVersion),
			zap.String("trace_id", env.TraceID),
		)
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		r.logger.Error("outbox sweep: commit", zap.Error(err))
		return
	}
	r.logger.Debug("outbox sweep done", zap.Int("published", published), zap.Int("scanned", len(rows)))
}

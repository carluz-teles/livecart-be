// Package dbtx centralizes the "run a unit of work in one transaction" dance so
// callers stop copying begin / defer-rollback / commit into every method.
package dbtx

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
)

// InTx runs fn inside a single database transaction: it opens the tx, hands fn a
// tx-scoped *sqlc.Queries, commits when fn returns nil and rolls back on any
// error (or panic). It is the single maintenance point for transactional work —
// most callers pair a state mutation with an events.Emit on the same handle, so
// the state change and its outbox event commit atomically "by default".
//
//	err := dbtx.InTx(ctx, pool, queries, func(q *sqlc.Queries) error {
//	    row, err := q.CreateThing(ctx, ...)
//	    if err != nil { return err }
//	    return events.EmitInternal(ctx, q, events.ThingCreated, "thing.created:"+row.ID, payload)
//	})
func InTx(ctx context.Context, pool *pgxpool.Pool, queries *sqlc.Queries, fn func(q *sqlc.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit; rolls back on early return/panic

	if err := fn(queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

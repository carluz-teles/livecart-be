package integration

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/lib/httpx"
)

// Joins are rare and hold a session during ERP I/O. Keep their connections out
// of the query pool so nested finalisation calls cannot exhaust that pool.
var joinLockSlots = make(chan struct{}, 2)

func (r *Repository) lockCartJoin(ctx context.Context, ids ...string) (func(), error) {
	select {
	case joinLockSlots <- struct{}{}:
	default:
		return nil, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "há junções em andamento; tente novamente")
	}
	connectCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	conn, err := pgx.ConnectConfig(connectCtx, r.pool.Config().ConnConfig.Copy())
	if err != nil {
		<-joinLockSlots
		return nil, err
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanup) // Closing the dedicated session also drops its locks.
		<-joinLockSlots
	}
	sort.Strings(ids)
	for _, id := range ids {
		var acquired bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, "cart_topology:"+id).Scan(&acquired); err != nil {
			release()
			return nil, err
		}
		if !acquired {
			release()
			return nil, httpx.DomainError(409, httpx.CodeCartERPSyncPending, "um dos pedidos está sendo alterado; tente novamente")
		}
	}
	return release, nil
}

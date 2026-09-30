package cartedit

import (
	"context"
	"sort"

	"livecart/apps/api/lib/httpx"
)

// The transaction keeps these shared locks through acceptance. A join holds
// their exclusive counterparts through its ERP work and possible undo.
func LockTopologyForEdit(ctx context.Context, db Reader, ids ...string) error {
	sort.Strings(ids)
	for _, id := range ids {
		var acquired bool
		if err := db.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`, "cart_topology:"+id).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			return httpx.DomainError(409, httpx.CodeCartERPSyncPending, "os pedidos estão sendo unidos; aguarde para editar")
		}
	}
	return nil
}

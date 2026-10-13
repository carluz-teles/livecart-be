// Package cartedit contains the shared contract for durable merchant edits.
package cartedit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/lib/httpx"
)

type requestKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey{}, id)
}
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestKey{}).(string)
	return id
}

type Reader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Status struct {
	Pending     bool   `json:"pending"`
	Processing  bool   `json:"processing"`
	Blocked     bool   `json:"blocked"`
	LastError   string `json:"lastError,omitempty"`
	Attempts    int    `json:"attempts"`
	BlockReason string `json:"blockReason,omitempty"`
}

func Read(ctx context.Context, db Reader, cartID string) (*Status, error) {
	var s Status
	err := db.QueryRow(ctx, `SELECT COALESCE(w.revision>w.synced_revision,false) OR b.cart_id IS NOT NULL,
        COALESCE(w.lease_until>now(),false),COALESCE(w.last_error,''),COALESCE(w.attempts,0),
        COALESCE(w.blocked_at IS NOT NULL AND w.revision>w.synced_revision,false) OR b.cart_id IS NOT NULL,
        CASE WHEN b.cart_id IS NOT NULL THEN 'accounts_launched' ELSE '' END
        FROM carts c LEFT JOIN cart_erp_edits w ON w.cart_id=COALESCE(c.joined_to_cart_id,c.id)
        LEFT JOIN active_cart_erp_item_blocks b ON b.cart_id=COALESCE(c.joined_to_cart_id,c.id)
        WHERE c.id=$1`, cartID).
		Scan(&s.Pending, &s.Processing, &s.LastError, &s.Attempts, &s.Blocked, &s.BlockReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return &s, nil
	}
	return &s, err
}

func AssertReady(ctx context.Context, db Reader, cartID string) error {
	s, err := Read(ctx, db, cartID)
	if err != nil {
		return err
	}
	if s.Pending {
		if s.Blocked {
			return httpx.DomainError(409, httpx.CodeCartERPSyncPending,
				"as alterações deste pedido precisam de conciliação com o ERP antes de continuar")
		}
		return httpx.DomainError(409, httpx.CodeCartERPSyncPending, "o pedido está sincronizando alterações; aguarde a confirmação antes de continuar")
	}
	return nil
}

// PendingProducts survives deletion of a cart_items row, including orders
// created before LiveCart started marking each ERP line.
func PendingProducts(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, cartID string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT p.external_id FROM cart_erp_edit_requests r
        JOIN products p ON p.id=r.product_id JOIN cart_erp_edits w ON w.cart_id=r.cart_id
        WHERE r.cart_id=(SELECT COALESCE(joined_to_cart_id,id) FROM carts WHERE id=$1) AND r.revision>w.synced_revision AND p.external_id IS NOT NULL`, cartID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

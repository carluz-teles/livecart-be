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
	Pending    bool   `json:"pending"`
	Processing bool   `json:"processing"`
	LastError  string `json:"lastError,omitempty"`
	Attempts   int    `json:"attempts"`
}

func Read(ctx context.Context, db Reader, cartID string) (*Status, error) {
	var s Status
	err := db.QueryRow(ctx, `SELECT revision>synced_revision,
        COALESCE(lease_until>now(),false),COALESCE(last_error,''),attempts
        FROM cart_erp_edits WHERE cart_id=$1`, cartID).
		Scan(&s.Pending, &s.Processing, &s.LastError, &s.Attempts)
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
        WHERE r.cart_id=$1 AND r.revision>w.synced_revision AND p.external_id IS NOT NULL`, cartID)
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

package cartedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrReconciliation = errors.New("alteração requer conferência")

type Execution struct {
	Version       int    `json:"version"`
	IntegrationID string `json:"integrationId,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Remote        bool   `json:"remote"`
}

func ReadExecution(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, cartID string, revision int64) (Execution, error) {
	rows, err := db.Query(ctx, `SELECT request->'_execution' FROM cart_erp_edit_requests r
 JOIN cart_erp_edits w ON w.cart_id=r.cart_id WHERE r.cart_id=$1 AND r.revision>w.synced_revision AND r.revision<=$2`, cartID, revision)
	if err != nil {
		return Execution{}, err
	}
	defer rows.Close()
	var result Execution
	count := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return result, err
		}
		var current Execution
		if len(raw) == 0 || json.Unmarshal(raw, &current) != nil || current.Version != 1 ||
			(current.Remote && (current.IntegrationID == "" || current.Provider == "")) {
			return result, fmt.Errorf("missing or invalid ERP execution binding: %w", ErrReconciliation)
		}
		if count > 0 && current != result {
			return result, fmt.Errorf("pending edits refer to different integrations: %w", ErrReconciliation)
		}
		result = current
		count++
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if count == 0 {
		return result, fmt.Errorf("pending edit has no requests: %w", ErrReconciliation)
	}
	return result, nil
}

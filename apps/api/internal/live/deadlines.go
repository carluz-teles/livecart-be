package live

import (
	"context"
	"fmt"
	"time"

	"livecart/apps/api/internal/events"
)

// ApplyEventDeadlineSettings commits config, monotonic deadline propagation and
// the durable scheduling events together. A queue outage cannot lose an increase.
func (r *Repository) ApplyEventDeadlineSettings(ctx context.Context, eventID, storeID string, x, y *int) ([]string, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	if y != nil {
		minutes := max(0, min(*y, 43200))
		y = &minutes
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin deadline settings transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Updating the event takes its lock before the carts, the same order used by
	// commercial close. Concurrent configuration edits therefore cannot race.
	if _, err := tx.Exec(ctx, `
		UPDATE live_events
		SET cart_expiration_minutes = COALESCE($3::int, cart_expiration_minutes),
		    waitlist_notified_ttl_minutes = COALESCE($4::int, waitlist_notified_ttl_minutes),
		    updated_at = now()
		WHERE id = $1 AND store_id = $2`, uid, storeUID, x, y); err != nil {
		return nil, fmt.Errorf("updating event deadline settings: %w", err)
	}
	qtx := r.q.WithTx(tx)
	rows, err := qtx.ShiftOpenCartExpirations(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("extending open cart deadlines: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, id := range rows {
		cartID := id.String()
		ids = append(ids, cartID)
		key := fmt.Sprintf("cart.checkout_armed:deadline:%s:%d", cartID, time.Now().UnixNano())
		if err := emitCartEvent(ctx, qtx, events.CartCheckoutArmed, cartID, eventID, "", key); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit event deadline settings: %w", err)
	}
	return ids, nil
}

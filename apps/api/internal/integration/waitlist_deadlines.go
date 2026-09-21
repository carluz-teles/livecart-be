package integration

import (
	"context"
	"fmt"
	"time"
)

// GetNextEventWaitlistDeadline excludes VIP, paid and reviewed carts. Their
// pending quantities must never be ended by the event timer.
func (r *Repository) GetNextEventWaitlistDeadline(ctx context.Context, eventID string) (*time.Time, error) {
	var deadline *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT min(c.expires_at)
		FROM carts c
		WHERE c.status IN ('active', 'checkout')
		  AND NOT c.never_expires
		  AND NOT c.payment_review_required
		  AND c.payment_status IS DISTINCT FROM 'paid'
		  AND c.payment_status IS DISTINCT FROM 'refunded'
		  AND EXISTS (
		      SELECT 1 FROM waitlist_items wi
		      WHERE wi.cart_id = c.id AND wi.event_id = $1::uuid
		        AND wi.status = 'waiting'
		  )`, eventID).Scan(&deadline)
	if err != nil {
		return nil, fmt.Errorf("reading event waitlist deadline: %w", err)
	}
	return deadline, nil
}

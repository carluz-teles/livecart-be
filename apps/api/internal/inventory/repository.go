// Package inventory owns the inventory-side reconciliation of live-commerce
// facts — currently the waitlist fulfilment that reacts to cart.paid (Fatia A2).
// The event reactor lives in internal/inventory/listeners; this file is the
// repository it delegates to (the WaitlistFulfiller implementation).
package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
)

// Repository is a thin wrapper over sqlc for the inventory reactor. Owning a
// separate repo keeps the package importable without dragging another domain's
// dependency graph — and avoids an import cycle with postcheckout, where this
// fulfilment used to live before it became a domain reactor.
type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

// MarkWaitlistFulfilledByCart flips every 'notified' waitlist row tied to this
// cart to 'fulfilled' (the buyer paid within the notification window) and emits
// one waitlist.fulfilled per affected row. Rows in 'waiting' — items the customer
// chose to leave queued — are left untouched (WHERE status='notified' na query
// canônica MarkWaitlistFulfilledByCart, reusada, não reimplementada).
//
// Idempotent by construction: a redelivery finds 0 rows in 'notified' → the
// UPDATE returns 0 rows → 0 emissions. Relocated as-is from postcheckout so there
// is a single fulfilment+emission site, now owned by the Inventory domain.
func (r *Repository) MarkWaitlistFulfilledByCart(ctx context.Context, cartID string) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return fmt.Errorf("parsing cart id: %w", err)
	}
	fulfilled, err := r.q.MarkWaitlistFulfilledByCart(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return fmt.Errorf("marking waitlist fulfilled by cart %s: %w", cartID, err)
	}

	// Emit waitlist.fulfilled per affected row (keyed by waitlist_item_id).
	// Best-effort: the fulfill flip already committed and this observability event
	// must not fail the paid flow, so outbox-insert errors are swallowed (there is
	// no logger at this layer).
	for _, item := range fulfilled {
		itemID := uuid.UUID(item.ID.Bytes).String()
		_ = events.EmitInternal(ctx, r.q, events.WaitlistFulfilled, "waitlist.fulfilled:"+itemID, struct {
			WaitlistItemID string `json:"waitlist_item_id"`
			CartID         string `json:"cart_id"`
			ProductID      string `json:"product_id"`
			EventID        string `json:"event_id"`
		}{
			WaitlistItemID: itemID,
			CartID:         cartID,
			ProductID:      uuid.UUID(item.ProductID.Bytes).String(),
			EventID:        uuid.UUID(item.EventID.Bytes).String(),
		})
	}
	return nil
}

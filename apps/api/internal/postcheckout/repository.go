package postcheckout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

// timeNow exists so tests can override clock without a clock injection across
// the call surface.
var timeNow = func() time.Time { return time.Now() }

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// Repository is a thin wrapper over sqlc focused on the post-checkout flow.
// Owning a separate repo (rather than reusing checkout's) keeps the package
// importable without dragging the checkout service's full dependency graph.
type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

// CartSnapshot is the slim aggregate the postcheckout service needs to render
// a receipt: the cart row itself, line items with product names, the live
// event (for store_id), and the store (for branding).
type CartSnapshot struct {
	Cart  sqlc.Cart
	Items []sqlc.ListCartItemsRow
	Event sqlc.LiveEvent
	Store sqlc.Store
}

// LoadCart hydrates everything needed to render the order paid email in one
// call. Returns an error wrapping pgx.ErrNoRows if any layer is missing.
func (r *Repository) LoadCart(ctx context.Context, cartID string) (*CartSnapshot, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, fmt.Errorf("parsing cart id: %w", err)
	}
	cartUID := pgtype.UUID{Bytes: uid, Valid: true}

	cart, err := r.q.GetCartByID(ctx, cartUID)
	if err != nil {
		return nil, fmt.Errorf("loading cart: %w", err)
	}

	items, err := r.q.ListCartItems(ctx, cartUID)
	if err != nil {
		return nil, fmt.Errorf("loading cart items: %w", err)
	}

	event, err := r.q.GetLiveEventByID(ctx, cart.EventID)
	if err != nil {
		return nil, fmt.Errorf("loading event: %w", err)
	}

	store, err := r.q.GetStoreByID(ctx, event.StoreID)
	if err != nil {
		return nil, fmt.Errorf("loading store: %w", err)
	}

	return &CartSnapshot{
		Cart:  cart,
		Items: items,
		Event: event,
		Store: store,
	}, nil
}

// GetCartByTrackingToken does a single-shot lookup by token (globally unique
// after migration 000066). Returns nil cart and nil error when not found, so
// the public handler can answer 404 without leaking a different signal for
// "exists but token wrong" vs "doesn't exist".
func (r *Repository) GetCartByTrackingToken(ctx context.Context, token string) (*sqlc.Cart, error) {
	if token == "" {
		return nil, nil
	}
	cart, err := r.q.GetCartByTrackingToken(ctx, pgtype.Text{String: token, Valid: true})
	if err != nil {
		return nil, nil
	}
	return &cart, nil
}

// SetTrackingToken persists the generated tracking_token. Idempotency lives
// at the call site: the service skips this when cart.TrackingToken is already
// set.
func (r *Repository) SetTrackingToken(ctx context.Context, cartID, token string) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return fmt.Errorf("parsing cart id: %w", err)
	}
	return r.q.SetCartTrackingToken(ctx, sqlc.SetCartTrackingTokenParams{
		ID:            pgtype.UUID{Bytes: uid, Valid: true},
		TrackingToken: pgtype.Text{String: token, Valid: true},
	})
}

// InsertOrderEvent appends an event to the customer-facing timeline.
// The DB enforces (cart_id, event_type) uniqueness so retried webhooks/hooks
// don't duplicate emails. Returns true when the row was newly inserted (so
// the caller can decide whether to fire side effects like emails).
func (r *Repository) InsertOrderEvent(ctx context.Context, cartID, eventType, source string, metadata json.RawMessage) (bool, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return false, fmt.Errorf("parsing cart id: %w", err)
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	_, err = r.q.InsertOrderEvent(ctx, sqlc.InsertOrderEventParams{
		CartID:     pgtype.UUID{Bytes: uid, Valid: true},
		EventType:  eventType,
		OccurredAt: pgtype.Timestamptz{Time: timeNow(), Valid: true},
		Source:     source,
		Metadata:   metadata,
	})
	// pgx returns ErrNoRows when ON CONFLICT DO NOTHING swallows the insert,
	// which means an event of this type already exists. That's the
	// idempotency path — not an error.
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("inserting order event: %w", err)
	}
	return true, nil
}

// ListOrderEvents returns the cart's timeline ordered chronologically.
func (r *Repository) ListOrderEvents(ctx context.Context, cartID string) ([]sqlc.OrderEvent, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, fmt.Errorf("parsing cart id: %w", err)
	}
	return r.q.ListOrderEventsByCart(ctx, pgtype.UUID{Bytes: uid, Valid: true})
}

// ShipmentSummary is the slim view the public order page needs from the
// shipments table. The full integration package owns shipments via raw SQL
// — this small read keeps us from depending on that repository.
type ShipmentSummary struct {
	TrackingCode string
}

// SQLRunner is the minimal pool surface we need (so callers pass either
// pgxpool.Pool or a transaction).
type SQLRunner interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GetShipmentSummary returns the most-recent shipment row for a cart, or nil
// when none exists yet. Returns an empty TrackingCode when the merchant
// created a shipment but the carrier hasn't issued a code.
func (r *Repository) GetShipmentSummary(ctx context.Context, runner SQLRunner, cartID string) (*ShipmentSummary, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, fmt.Errorf("parsing cart id: %w", err)
	}
	row := runner.QueryRow(ctx,
		`SELECT tracking_code FROM shipments WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1`,
		uid,
	)
	var tracking pgtype.Text
	if err := row.Scan(&tracking); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading shipment summary: %w", err)
	}
	if !tracking.Valid {
		return &ShipmentSummary{}, nil
	}
	return &ShipmentSummary{TrackingCode: tracking.String}, nil
}

// ShippingAddressJSON is the minimal shape we read from the cart's
// shipping_address JSONB. Mirrors the FE shape the customer typed at
// checkout.
type ShippingAddressJSON struct {
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
	ZipCode      string `json:"zipCode"`
}

func ParseShippingAddress(raw json.RawMessage) ShippingAddressJSON {
	var out ShippingAddressJSON
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

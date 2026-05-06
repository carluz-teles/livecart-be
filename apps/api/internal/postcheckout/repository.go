package postcheckout

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

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

package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

// Repository handles database operations for checkout.
type Repository struct {
	q *sqlc.Queries
}

// NewRepository creates a new checkout repository.
func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

// GetCartByToken retrieves a cart by its token with event and store info.
func (r *Repository) GetCartByToken(ctx context.Context, token string) (*CartRow, error) {
	row, err := r.q.GetCartByTokenWithDetails(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("carrinho não encontrado")
		}
		return nil, fmt.Errorf("getting cart by token: %w", err)
	}

	return r.toCartRow(row), nil
}

// ListCartItems retrieves all items for a cart.
func (r *Repository) ListCartItems(ctx context.Context, cartID string) ([]CartItemRow, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid cart ID")
	}

	rows, err := r.q.ListCartItemsForCheckout(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("listing cart items: %w", err)
	}

	items := make([]CartItemRow, len(rows))
	for i, row := range rows {
		items[i] = r.toCartItemRow(row)
	}

	return items, nil
}

// UpdateCustomerEmail updates the customer email for a cart.
func (r *Repository) UpdateCustomerEmail(ctx context.Context, token, email string) error {
	_, err := r.q.UpdateCartCustomerEmail(ctx, sqlc.UpdateCartCustomerEmailParams{
		Token:         token,
		CustomerEmail: pgtype.Text{String: email, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("carrinho não encontrado")
		}
		return fmt.Errorf("updating customer email: %w", err)
	}
	return nil
}

// UpdateCheckoutInfo updates the checkout URL and ID for a cart.
func (r *Repository) UpdateCheckoutInfo(ctx context.Context, params UpdateCheckoutParams) error {
	uid, err := uuid.Parse(params.CartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}

	var expiresAt pgtype.Timestamptz
	if params.CheckoutExpiresAt != nil {
		expiresAt = pgtype.Timestamptz{Time: *params.CheckoutExpiresAt, Valid: true}
	}

	_, err = r.q.UpdateCartCheckoutInfo(ctx, sqlc.UpdateCartCheckoutInfoParams{
		ID:                pgtype.UUID{Bytes: uid, Valid: true},
		CheckoutUrl:       pgtype.Text{String: params.CheckoutURL, Valid: true},
		CheckoutID:        pgtype.Text{String: params.CheckoutID, Valid: true},
		CheckoutExpiresAt: expiresAt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("carrinho não encontrado")
		}
		return fmt.Errorf("updating checkout info: %w", err)
	}
	return nil
}

// GetPaymentIntegration retrieves the active payment integration for a store.
func (r *Repository) GetPaymentIntegration(ctx context.Context, storeID string) (*IntegrationRow, error) {
	uid, err := uuid.Parse(storeID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid store ID")
	}

	row, err := r.q.GetStorePaymentIntegration(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // No payment integration configured
		}
		return nil, fmt.Errorf("getting payment integration: %w", err)
	}

	return &IntegrationRow{
		ID:       row.ID.Bytes,
		StoreID:  row.StoreID.Bytes,
		Type:     row.Type,
		Provider: row.Provider,
		Status:   row.Status,
	}, nil
}

// IntegrationRow represents a minimal integration row.
type IntegrationRow struct {
	ID       uuid.UUID
	StoreID  uuid.UUID
	Type     string
	Provider string
	Status   string
}

// =============================================================================
// HELPERS
// =============================================================================

func (r *Repository) toCartRow(row sqlc.GetCartByTokenWithDetailsRow) *CartRow {
	var eventTitle string
	if row.EventTitle.Valid {
		eventTitle = row.EventTitle.String
	}

	cart := &CartRow{
		ID:             uuid.UUID(row.ID.Bytes).String(),
		EventID:        uuid.UUID(row.EventID.Bytes).String(),
		PlatformUserID: row.PlatformUserID,
		PlatformHandle: row.PlatformHandle,
		Token:          row.Token,
		Status:         row.Status,
		PaymentStatus:  "unpaid",
		CreatedAt:      row.CreatedAt.Time,
		EventTitle:     eventTitle,
		StoreID:        uuid.UUID(row.StoreID.Bytes).String(),
		StoreName:      row.StoreName,
		AllowEdit:      row.AllowEdit,
	}

	if row.CheckoutUrl.Valid {
		cart.CheckoutURL = &row.CheckoutUrl.String
	}
	if row.CheckoutID.Valid {
		cart.CheckoutID = &row.CheckoutID.String
	}
	if row.CheckoutExpiresAt.Valid {
		cart.CheckoutExpiresAt = &row.CheckoutExpiresAt.Time
	}
	if row.CustomerEmail.Valid {
		cart.CustomerEmail = &row.CustomerEmail.String
	}
	if row.PaymentStatus.Valid {
		cart.PaymentStatus = row.PaymentStatus.String
	}
	if row.PaidAt.Valid {
		cart.PaidAt = &row.PaidAt.Time
	}
	if row.ExpiresAt.Valid {
		cart.ExpiresAt = &row.ExpiresAt.Time
	}
	if row.StoreLogoUrl.Valid {
		cart.StoreLogoURL = &row.StoreLogoUrl.String
	}

	return cart
}

func (r *Repository) toCartItemRow(row sqlc.ListCartItemsForCheckoutRow) CartItemRow {
	item := CartItemRow{
		ID:         uuid.UUID(row.ID.Bytes).String(),
		CartID:     uuid.UUID(row.CartID.Bytes).String(),
		ProductID:  uuid.UUID(row.ProductID.Bytes).String(),
		Quantity:   int(row.Quantity.Int32),
		UnitPrice:  row.UnitPrice.Int64,
		Waitlisted: row.Waitlisted.Bool,
		Name:       row.ProductName,
	}

	if row.ProductImageUrl.Valid {
		item.ImageURL = &row.ProductImageUrl.String
	}
	if row.ProductKeyword != "" {
		item.Keyword = &row.ProductKeyword
	}

	return item
}

// GetStoreCheckoutExpiryHours returns the checkout link expiry hours for a store.
func (r *Repository) GetStoreCheckoutExpiryHours(ctx context.Context, storeID string) (int, error) {
	uid, err := uuid.Parse(storeID)
	if err != nil {
		return 48, nil // Default to 48 hours
	}

	row, err := r.q.GetStoreByID(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return 48, nil // Default to 48 hours
	}

	if row.CheckoutLinkExpiryHours.Valid {
		return int(row.CheckoutLinkExpiryHours.Int32), nil
	}

	return 48, nil // Default
}

// CalculateCartTotal calculates the total for non-waitlisted items.
func CalculateCartTotal(items []CartItemRow) (subtotal int64, totalItems int) {
	for _, item := range items {
		if !item.Waitlisted {
			subtotal += item.UnitPrice * int64(item.Quantity)
			totalItems += item.Quantity
		}
	}
	return
}

// GetExpiresAt calculates the expiration time based on store settings.
func GetExpiresAt(hours int) *time.Time {
	if hours <= 0 {
		return nil
	}
	t := time.Now().Add(time.Duration(hours) * time.Hour)
	return &t
}

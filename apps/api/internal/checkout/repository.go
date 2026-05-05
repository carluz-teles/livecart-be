package checkout

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

// UpdateCheckoutCustomer persists the customer identity and shipping address
// entered in the transparent checkout form. These fields are required to later
// create the paid sales order in the ERP when the payment webhook confirms.
func (r *Repository) UpdateCheckoutCustomer(ctx context.Context, cartID, email, name, document, phone string, address *ShippingAddress) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}

	var addressJSON json.RawMessage
	if address != nil {
		b, err := json.Marshal(address)
		if err != nil {
			return fmt.Errorf("marshaling shipping address: %w", err)
		}
		addressJSON = b
	}

	_, err = r.q.UpdateCartCustomerCheckout(ctx, sqlc.UpdateCartCustomerCheckoutParams{
		ID:               pgtype.UUID{Bytes: uid, Valid: true},
		CustomerEmail:    pgtype.Text{String: email, Valid: email != ""},
		CustomerName:     pgtype.Text{String: name, Valid: name != ""},
		CustomerDocument: pgtype.Text{String: document, Valid: document != ""},
		CustomerPhone:    pgtype.Text{String: phone, Valid: phone != ""},
		ShippingAddress:  addressJSON,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("carrinho não encontrado")
		}
		return fmt.Errorf("updating checkout customer: %w", err)
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

// UpdatePaymentStatus updates the payment status for a cart. When status is
// "paid", paidAt is the authoritative authorization instant — typically the
// gateway-reported value (MP date_approved, Pagar.me charges[0].paid_at) so
// the receipt matches what the customer sees on the gateway dashboard. Pass
// nil to fall back to time.Now() (the gateway omitted the field).
func (r *Repository) UpdatePaymentStatus(ctx context.Context, cartID, status, paymentID string, paidAt *time.Time) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}

	var paidAtPg pgtype.Timestamptz
	if status == "paid" {
		ts := time.Now()
		if paidAt != nil && !paidAt.IsZero() {
			ts = *paidAt
		}
		paidAtPg = pgtype.Timestamptz{Time: ts, Valid: true}
	}

	_, err = r.q.UpdateCartPaymentStatus(ctx, sqlc.UpdateCartPaymentStatusParams{
		ID:            pgtype.UUID{Bytes: uid, Valid: true},
		PaymentStatus: pgtype.Text{String: status, Valid: true},
		CheckoutID:    pgtype.Text{String: paymentID, Valid: true},
		PaidAt:        paidAtPg,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("carrinho não encontrado")
		}
		return fmt.Errorf("updating payment status: %w", err)
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
		ID:           row.ID.Bytes,
		StoreID:      row.StoreID.Bytes,
		Type:         row.Type,
		Provider:     row.Provider,
		ProviderName: row.Provider,
		Status:       row.Status,
	}, nil
}

// IntegrationRow represents a minimal integration row.
type IntegrationRow struct {
	ID           uuid.UUID
	StoreID      uuid.UUID
	Type         string
	Provider     string
	ProviderName string // Alias for Provider for convenience
	Status       string
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
		ID:                 uuid.UUID(row.ID.Bytes).String(),
		EventID:            uuid.UUID(row.EventID.Bytes).String(),
		PlatformUserID:     row.PlatformUserID,
		PlatformHandle:     row.PlatformHandle,
		Token:              row.Token,
		Status:             row.Status,
		PaymentStatus:      "unpaid",
		CreatedAt:          row.CreatedAt.Time,
		EventTitle:         eventTitle,
		StoreID:            uuid.UUID(row.StoreID.Bytes).String(),
		StoreName:          row.StoreName,
		AllowEdit:          row.AllowEdit,
		MaxQuantityPerItem: int(row.MaxQuantityPerItem),
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
	if row.CouponID.Valid {
		id := uuid.UUID(row.CouponID.Bytes).String()
		cart.CouponID = &id
	}
	if row.CouponCode.Valid {
		cart.CouponCode = &row.CouponCode.String
	}
	cart.CouponDiscountCents = row.CouponDiscountCents

	return cart
}

func (r *Repository) toCartItemRow(row sqlc.ListCartItemsForCheckoutRow) CartItemRow {
	item := CartItemRow{
		ID:                 uuid.UUID(row.ID.Bytes).String(),
		CartID:             uuid.UUID(row.CartID.Bytes).String(),
		ProductID:          uuid.UUID(row.ProductID.Bytes).String(),
		Quantity:           int(row.Quantity.Int32),
		UnitPrice:          row.UnitPrice.Int64,
		WaitlistedQuantity: int(row.WaitlistedQuantity),
		Name:               row.ProductName,
		AvailableStock:     int(row.ProductStock.Int32),
	}

	if row.ProductImageUrl.Valid {
		item.ImageURL = &row.ProductImageUrl.String
	}
	if row.ProductKeyword != "" {
		item.Keyword = &row.ProductKeyword
	}

	return item
}

// =============================================================================
// CART ITEM MUTATIONS (buyer edits at checkout)
// =============================================================================

// GetCartItem fetches a single cart_items row by id (no product join).
func (r *Repository) GetCartItem(ctx context.Context, itemID string) (*CartItemRow, error) {
	uid, err := uuid.Parse(itemID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid item ID")
	}
	row, err := r.q.GetCartItem(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("item não encontrado")
		}
		return nil, fmt.Errorf("getting cart item: %w", err)
	}
	return &CartItemRow{
		ID:                 uuid.UUID(row.ID.Bytes).String(),
		CartID:             uuid.UUID(row.CartID.Bytes).String(),
		ProductID:          uuid.UUID(row.ProductID.Bytes).String(),
		Quantity:           int(row.Quantity.Int32),
		UnitPrice:          row.UnitPrice.Int64,
		WaitlistedQuantity: int(row.WaitlistedQuantity),
	}, nil
}

// SetCartItemQuantity overwrites the quantity of a cart_items row.
func (r *Repository) SetCartItemQuantity(ctx context.Context, itemID string, quantity int) error {
	uid, err := uuid.Parse(itemID)
	if err != nil {
		return httpx.ErrBadRequest("invalid item ID")
	}
	_, err = r.q.UpdateCartItem(ctx, sqlc.UpdateCartItemParams{
		ID:       pgtype.UUID{Bytes: uid, Valid: true},
		Quantity: pgtype.Int4{Int32: int32(quantity), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("updating cart item quantity: %w", err)
	}
	return nil
}

// DeleteCartItem removes a cart_items row.
func (r *Repository) DeleteCartItem(ctx context.Context, itemID string) error {
	uid, err := uuid.Parse(itemID)
	if err != nil {
		return httpx.ErrBadRequest("invalid item ID")
	}
	if err := r.q.DeleteCartItem(ctx, pgtype.UUID{Bytes: uid, Valid: true}); err != nil {
		return fmt.Errorf("deleting cart item: %w", err)
	}
	return nil
}

// EnsureInitialSnapshot freezes the current cart_items into cart_initial_items
// the first time it's called for the cart. Subsequent calls are no-ops.
func (r *Repository) EnsureInitialSnapshot(ctx context.Context, cartID string) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	if err := r.q.EnsureCartInitialSnapshot(ctx, pgtype.UUID{Bytes: uid, Valid: true}); err != nil {
		return fmt.Errorf("ensuring cart initial snapshot: %w", err)
	}
	return nil
}

// MutationParams describes one buyer-driven cart edit.
type MutationParams struct {
	CartID          string
	ProductID       string
	MutationType    string // item_added | item_removed | quantity_increased | quantity_decreased
	QuantityBefore  int
	QuantityAfter   int
	UnitPrice       int64
	Source          string // buyer_checkout (default), live_add, merchant
	ERPMovementID   string
}

// RecordMutation persists one row in cart_mutations.
func (r *Repository) RecordMutation(ctx context.Context, p MutationParams) error {
	cID, err := uuid.Parse(p.CartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	pID, err := uuid.Parse(p.ProductID)
	if err != nil {
		return httpx.ErrBadRequest("invalid product ID")
	}
	source := p.Source
	if source == "" {
		source = "buyer_checkout"
	}
	_, err = r.q.CreateCartMutation(ctx, sqlc.CreateCartMutationParams{
		CartID:         pgtype.UUID{Bytes: cID, Valid: true},
		ProductID:      pgtype.UUID{Bytes: pID, Valid: true},
		MutationType:   p.MutationType,
		QuantityBefore: int32(p.QuantityBefore),
		QuantityAfter:  int32(p.QuantityAfter),
		UnitPrice:      p.UnitPrice,
		Source:         source,
		ErpMovementID:  pgtype.Text{String: p.ERPMovementID, Valid: p.ERPMovementID != ""},
	})
	if err != nil {
		return fmt.Errorf("recording cart mutation: %w", err)
	}
	return nil
}

// GetEventProductForCart returns the effective price + max quantity + stock
// for a product in the cart's event. Used to validate quantity bumps.
type EventProductConfig struct {
	ProductID      string
	Name           string
	UnitPrice      int64
	MaxQuantity    int
	Stock          int
	Active         bool
	IsAllowed      bool // false when the event has a whitelist and this product is not in it
}

// GetEventProductForCart resolves event-aware pricing/limits for a product.
func (r *Repository) GetEventProductForCart(ctx context.Context, eventID, storeID, productID string) (*EventProductConfig, error) {
	eID, err := uuid.Parse(eventID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid event ID")
	}
	pID, err := uuid.Parse(productID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid product ID")
	}
	sID, err := uuid.Parse(storeID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid store ID")
	}
	cfg, err := r.q.GetEventProductConfig(ctx, sqlc.GetEventProductConfigParams{
		EventID: pgtype.UUID{Bytes: eID, Valid: true},
		ID:      pgtype.UUID{Bytes: pID, Valid: true},
		StoreID: pgtype.UUID{Bytes: sID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("produto não encontrado")
		}
		return nil, fmt.Errorf("getting event product config: %w", err)
	}
	maxQty, _ := r.q.GetEffectiveMaxQuantity(ctx, sqlc.GetEffectiveMaxQuantityParams{
		ID:        pgtype.UUID{Bytes: eID, Valid: true},
		ProductID: pgtype.UUID{Bytes: pID, Valid: true},
	})
	out := &EventProductConfig{
		ProductID:   uuid.UUID(cfg.ProductID.Bytes).String(),
		Name:        cfg.ProductName,
		UnitPrice:   cfg.EffectivePrice,
		MaxQuantity: int(maxQty),
		Stock:       int(cfg.ProductStock.Int32),
		Active:      cfg.ProductActive.Bool,
		IsAllowed:   cfg.IsAllowed,
	}
	return out, nil
}

// FindCartItemByProduct returns the existing cart item id (and current qty)
// for (cart, product), or an empty struct if none exists.
func (r *Repository) FindCartItemByProduct(ctx context.Context, cartID, productID string) (*CartItemRow, error) {
	items, err := r.ListCartItems(ctx, cartID)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.ProductID == productID {
			itCopy := it
			return &itCopy, nil
		}
	}
	return nil, nil
}

// CreateCartItem inserts a new cart_items row.
func (r *Repository) CreateCartItem(ctx context.Context, cartID, productID string, quantity int, unitPrice int64) (string, error) {
	cID, err := uuid.Parse(cartID)
	if err != nil {
		return "", httpx.ErrBadRequest("invalid cart ID")
	}
	pID, err := uuid.Parse(productID)
	if err != nil {
		return "", httpx.ErrBadRequest("invalid product ID")
	}
	row, err := r.q.CreateCartItem(ctx, sqlc.CreateCartItemParams{
		CartID:             pgtype.UUID{Bytes: cID, Valid: true},
		ProductID:          pgtype.UUID{Bytes: pID, Valid: true},
		Quantity:           pgtype.Int4{Int32: int32(quantity), Valid: true},
		UnitPrice:          pgtype.Int8{Int64: unitPrice, Valid: true},
		WaitlistedQuantity: 0,
	})
	if err != nil {
		return "", fmt.Errorf("creating cart item: %w", err)
	}
	return uuid.UUID(row.ID.Bytes).String(), nil
}

// Deprecated: Use GetStoreCartExpirationMinutes instead.
// GetStoreCartExpirationMinutes returns the cart expiration time in minutes.
func (r *Repository) GetStoreCartExpirationMinutes(ctx context.Context, storeID string) (int, error) {
	uid, err := uuid.Parse(storeID)
	if err != nil {
		return 30, nil // Default to 30 minutes
	}

	row, err := r.q.GetStoreByID(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return 30, nil // Default to 30 minutes
	}

	return int(row.CartExpirationMinutes), nil
}

// CalculateCartTotal calculates the total for available (non-waitlisted) items.
func CalculateCartTotal(items []CartItemRow) (subtotal int64, totalItems int) {
	for _, item := range items {
		// Available quantity = total quantity - waitlisted quantity
		availableQty := item.Quantity - item.WaitlistedQuantity
		if availableQty > 0 {
			subtotal += item.UnitPrice * int64(availableQty)
			totalItems += availableQty
		}
	}
	return
}

// GetExpiresAt calculates the expiration time based on store settings.
// GetExpiresAt calculates expiration time from hours.
// Deprecated: Use GetExpiresAtMinutes instead.
func GetExpiresAt(hours int) *time.Time {
	if hours <= 0 {
		return nil
	}
	t := time.Now().Add(time.Duration(hours) * time.Hour)
	return &t
}

// GetExpiresAtMinutes calculates expiration time from minutes.
func GetExpiresAtMinutes(minutes int) *time.Time {
	if minutes <= 0 {
		return nil
	}
	t := time.Now().Add(time.Duration(minutes) * time.Minute)
	return &t
}

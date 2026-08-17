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
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/dbtx"
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

// LoadEventCheckoutFlags reads the event-level columns that are not yet
// wired through sqlc — currently free_shipping and pix_discount_percent.
// Both are zero-valued when the event row vanishes; callers should treat a
// missing event as a non-fatal hydration miss.
func (r *Repository) LoadEventCheckoutFlags(ctx context.Context, pool *pgxpool.Pool, eventID string) (freeShipping bool, pixDiscountPercent int, err error) {
	uid, err := uuid.Parse(eventID)
	if err != nil {
		return false, 0, httpx.ErrBadRequest("invalid event ID")
	}
	var pct int32
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(free_shipping, false), COALESCE(pix_discount_percent, 0)
		FROM live_events WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}).Scan(&freeShipping, &pct)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("loading event checkout flags: %w", err)
	}
	return freeShipping, int(pct), nil
}

// LoadEventType devolve a espécie do evento ('live' | 'post' | 'reel' |
// 'story') para a tela do comprador escolher o vocabulário — "Live em
// andamento" contra "Promoção ativa".
//
// Lê live_sessions.type, NÃO live_events.type. Duas razões, e a segunda é a que
// importa: (1) a 000122 dropou live_events.type e este SELECT quebraria a tela
// do COMPRADOR antes de qualquer painel; (2) com a campanha mista o container
// não tem espécie única — quem manda é o que existe dentro dele.
//
// Precedência: qualquer sessão de live ganha ("Live em andamento" é verdade
// enquanto UMA estiver no ar, mesmo que a campanha tenha posts); senão, o tipo
// mais frequente. Evento sem sessão devolve "" — miss de hidratação não fatal,
// e o chamador cai no texto neutro.
func (r *Repository) LoadEventType(ctx context.Context, pool *pgxpool.Pool, eventID string) (string, error) {
	uid, err := uuid.Parse(eventID)
	if err != nil {
		return "", httpx.ErrBadRequest("invalid event ID")
	}
	var eventType string
	err = pool.QueryRow(ctx, `
		SELECT type
		FROM live_sessions
		WHERE event_id = $1
		GROUP BY type
		ORDER BY (type = 'live') DESC, count(*) DESC, type ASC
		LIMIT 1
	`, pgtype.UUID{Bytes: uid, Valid: true}).Scan(&eventType)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("loading event type: %w", err)
	}
	return eventType, nil
}

// LoadVariantInfo batch-loads the variant group name + option values for the
// given product ids (variant products only) so the checkout can render a short
// title + the chosen options instead of one giant product name. Best-effort:
// products without variants simply don't appear in the map.
func (r *Repository) LoadVariantInfo(ctx context.Context, pool *pgxpool.Pool, productIDs []string) (map[string]CartItemRow, error) {
	out := make(map[string]CartItemRow)
	if len(productIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT p.id::text, COALESCE(pg.name, ''), po.name, pov.value
		FROM products p
		JOIN product_groups pg ON pg.id = p.group_id
		JOIN product_variant_options pvo ON pvo.product_id = p.id
		JOIN product_option_values pov ON pov.id = pvo.option_value_id
		JOIN product_options po ON po.id = pov.option_id
		WHERE p.id::text = ANY($1)
		ORDER BY p.id, po.position, pov.position
	`, productIDs)
	if err != nil {
		return nil, fmt.Errorf("loading variant info: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid, groupName, optName, optValue string
		if err := rows.Scan(&pid, &groupName, &optName, &optValue); err != nil {
			return nil, fmt.Errorf("scanning variant info: %w", err)
		}
		info := out[pid]
		info.GroupName = groupName
		info.Variant = append(info.Variant, VariantOption{Option: optName, Value: optValue})
		out[pid] = info
	}
	return out, rows.Err()
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
func (r *Repository) UpdateCheckoutCustomer(ctx context.Context, cartID, email, name, document, phone string, address *ShippingAddress, whatsappConsent bool) error {
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
		// LGPD: consent only counts when a phone was actually provided.
		WhatsappConsent: whatsappConsent && phone != "",
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
// SetCartPixCharge guarda a cobranca PIX viva: o id que da para cancelar e o
// valor que o QR na mao do comprador cobra.
func (r *Repository) SetCartPixCharge(ctx context.Context, cartID, cancelID string, amountCents int64) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	return r.q.SetCartPixCharge(ctx, sqlc.SetCartPixChargeParams{
		ID:             pgtype.UUID{Bytes: uid, Valid: true},
		PixChargeID:    pgtype.Text{String: cancelID, Valid: cancelID != ""},
		PixAmountCents: pgtype.Int8{Int64: amountCents, Valid: true},
	})
}

// TakeCartPixCharge le E limpa a cobranca viva na mesma instrucao.
//
// Duas mutacoes simultaneas no mesmo carrinho tentariam cancelar a mesma
// cobranca duas vezes. Quem le aqui e o unico que recebe o id, entao a chamada
// ao gateway sai uma vez so. Devolve ("", 0, nil) quando nao havia nada.
func (r *Repository) TakeCartPixCharge(ctx context.Context, cartID string) (string, int64, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return "", 0, httpx.ErrBadRequest("invalid cart ID")
	}
	row, err := r.q.TakeCartPixCharge(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, nil
		}
		return "", 0, err
	}
	return row.PixChargeID.String, row.PixAmountCents.Int64, nil
}

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

// GetPaymentIntegration retrieves the highest-priority active payment
// integration for a store. Lower priority = primary. See
// ListStorePaymentIntegrations for the fallback chain.
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

// BindCartPaymentIntegration pins a cart to a specific payment integration so
// subsequent processing reuses the exact provider the FE was tokenized for.
// Skips the UPDATE when the same integration is already bound.
func (r *Repository) BindCartPaymentIntegration(ctx context.Context, cartID, integrationID string) error {
	cartUID, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	intUID, err := uuid.Parse(integrationID)
	if err != nil {
		return httpx.ErrBadRequest("invalid integration ID")
	}
	if err := r.q.BindCartPaymentIntegration(ctx, sqlc.BindCartPaymentIntegrationParams{
		ID:                   pgtype.UUID{Bytes: cartUID, Valid: true},
		PaymentIntegrationID: pgtype.UUID{Bytes: intUID, Valid: true},
	}); err != nil {
		return fmt.Errorf("binding cart payment integration: %w", err)
	}
	return nil
}

// ClearCartPaymentIntegration sets carts.payment_integration_id back to NULL so
// the next /config call walks resolveCheckoutIntegration's priority chain
// fresh. Called when the currently-bound provider returns a transport error
// (5xx/429): keeping the binding would trap the buyer on a gateway that's
// down, defeating the priority-chain fallback. Pairs with the FE re-tokenizing
// against the new provider's public key on the retry.
//
// Re-uses BindCartPaymentIntegration's generated query — pgtype.UUID with
// Valid=false is encoded as SQL NULL by pgx, so the same UPDATE writes NULL
// without needing a second SQLC query.
func (r *Repository) ClearCartPaymentIntegration(ctx context.Context, cartID string) error {
	cartUID, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	if err := r.q.BindCartPaymentIntegration(ctx, sqlc.BindCartPaymentIntegrationParams{
		ID:                   pgtype.UUID{Bytes: cartUID, Valid: true},
		PaymentIntegrationID: pgtype.UUID{Valid: false},
	}); err != nil {
		return fmt.Errorf("clearing cart payment integration: %w", err)
	}
	return nil
}

// GetIntegrationByID fetches a single integration row by ID (any type/status).
// Used when a cart was previously bound to a specific payment integration so
// the checkout service can reuse it instead of re-resolving the store primary.
func (r *Repository) GetIntegrationByID(ctx context.Context, integrationID string) (*IntegrationRow, error) {
	uid, err := uuid.Parse(integrationID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid integration ID")
	}
	row, err := r.q.GetIntegrationByIDOnly(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting integration: %w", err)
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

// ListPaymentIntegrations returns all active payment integrations for a store
// in priority order. The checkout service walks this slice as a fallback chain
// — if the primary fails to instantiate, the next entry is tried.
func (r *Repository) ListPaymentIntegrations(ctx context.Context, storeID string) ([]IntegrationRow, error) {
	uid, err := uuid.Parse(storeID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid store ID")
	}

	rows, err := r.q.ListStorePaymentIntegrations(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("listing payment integrations: %w", err)
	}

	out := make([]IntegrationRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, IntegrationRow{
			ID:           row.ID.Bytes,
			StoreID:      row.StoreID.Bytes,
			Type:         row.Type,
			Provider:     row.Provider,
			ProviderName: row.Provider,
			Status:       row.Status,
		})
	}
	return out, nil
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
		ID:                  uuid.UUID(row.ID.Bytes).String(),
		EventID:             uuid.UUID(row.EventID.Bytes).String(),
		PlatformUserID:      row.PlatformUserID,
		PlatformHandle:      row.PlatformHandle,
		Token:               row.Token,
		Status:              row.Status,
		PaymentStatus:       "pending",
		CreatedAt:           row.CreatedAt.Time,
		EventTitle:          eventTitle,
		StoreID:             uuid.UUID(row.StoreID.Bytes).String(),
		StoreName:           row.StoreName,
		StoreSlug:           row.StoreSlug,
		AllowEdit:           row.AllowEdit,
		MaxQuantityPerItem:  int(row.MaxQuantityPerItem),
		MinInstallmentCents: int(row.MinInstallmentCents),
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
	if row.PaymentIntegrationID.Valid {
		id := uuid.UUID(row.PaymentIntegrationID.Bytes).String()
		cart.PaymentIntegrationID = &id
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
	if row.CancelledReason.Valid {
		cart.CancelledReason = row.CancelledReason.String
	}

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
// SetCartItemSplitIfUnchanged grava total + parte em fila, e só se a linha
// ainda estiver como a lemos. Devolve false quando outro escritor chegou antes
// (0 linhas afetadas) — ver a query para o porquê das duas coisas juntas.
func (r *Repository) SetCartItemSplitIfUnchanged(ctx context.Context, itemID string, expectedQty, quantity, waitlisted int) (bool, error) {
	uid, err := uuid.Parse(itemID)
	if err != nil {
		return false, httpx.ErrBadRequest("invalid item ID")
	}
	n, err := r.q.SetCartItemSplitIfUnchanged(ctx, sqlc.SetCartItemSplitIfUnchangedParams{
		ID:                 pgtype.UUID{Bytes: uid, Valid: true},
		ExpectedQuantity:   int32(expectedQty),
		Quantity:           int32(quantity),
		WaitlistedQuantity: int32(waitlisted),
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

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
	CartID         string
	ProductID      string
	MutationType   string // item_added | item_removed | quantity_increased | quantity_decreased
	QuantityBefore int
	QuantityAfter  int
	UnitPrice      int64
	Source         string // buyer_checkout (default), live_add, merchant
	ERPMovementID  string
}

// RecordMutation persists one row in cart_mutations and, in the SAME
// transaction, emits the canonical cart-item event for the mutation (group C:
// cart.item_added / cart.item_qty_changed / cart.item_removed). The event is
// keyed by the mutation row id, which is the catalog idempotency key. Every
// checkout item edit (add/qty/remove) funnels through here, so this is the
// single canonical emission point for buyer-checkout cart mutations.
func (r *Repository) RecordMutation(ctx context.Context, pool *pgxpool.Pool, p MutationParams) error {
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

	// Record the mutation and, when it maps to a canonical event, emit it in the
	// SAME tx (keyed by the mutation id) via the shared runner.
	return dbtx.InTx(ctx, pool, r.q, func(q *sqlc.Queries) error {
		mutation, err := q.CreateCartMutation(ctx, sqlc.CreateCartMutationParams{
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

		name := cartMutationEventName(p.MutationType)
		if name == "" {
			return nil
		}
		mutationID := uuid.UUID(mutation.ID.Bytes).String()
		return events.EmitInternal(ctx, q, name, string(name)+":"+mutationID, struct {
			MutationID     string `json:"mutation_id"`
			CartID         string `json:"cart_id"`
			ProductID      string `json:"product_id"`
			MutationType   string `json:"mutation_type"`
			QuantityBefore int    `json:"quantity_before"`
			QuantityAfter  int    `json:"quantity_after"`
			Source         string `json:"source"`
		}{
			MutationID:     mutationID,
			CartID:         p.CartID,
			ProductID:      p.ProductID,
			MutationType:   p.MutationType,
			QuantityBefore: p.QuantityBefore,
			QuantityAfter:  p.QuantityAfter,
			Source:         source,
		})
	})
}

// cartMutationEventName maps a cart_mutations.mutation_type to its canonical
// group-C event name. Only item_added has a live event; the quantity-change and
// item-removed telemetry facts were deprecated (high-cardinality, no reactor).
// Returns "" for types with no event.
func cartMutationEventName(mutationType string) events.Name {
	switch mutationType {
	case "item_added":
		return events.CartItemAdded
	default:
		return ""
	}
}

// GetEventProductForCart returns the effective price + max quantity + stock
// for a product in the cart's event. Used to validate quantity bumps.
type EventProductConfig struct {
	ProductID   string
	Name        string
	UnitPrice   int64
	MaxQuantity int
	Stock       int
	Active      bool
	IsAllowed   bool // false when the event has a whitelist and this product is not in it
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
	// D15/N2: a whitelist é da SESSÃO, mas o carrinho é do EVENTO e atravessa N
	// sessões — não existe "a sessão do checkout". A regra é a união: o produto
	// é aceito se ALGUMA sessão do evento o aceita, e sessão sem whitelist
	// aceita tudo.
	cfg, err := r.q.GetEventProductConfigFromSessions(ctx, sqlc.GetEventProductConfigFromSessionsParams{
		EventID: pgtype.UUID{Bytes: eID, Valid: true},
		ID:      pgtype.UUID{Bytes: pID, Valid: true},
		StoreID: pgtype.UUID{Bytes: sID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("produto não encontrado")
		}
		return nil, fmt.Errorf("getting session product config: %w", err)
	}
	maxQty, _ := r.q.GetEffectiveMaxQuantityFromSessions(ctx, sqlc.GetEffectiveMaxQuantityFromSessionsParams{
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
		IsAllowed:   cfg.IsAllowed.Bool,
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

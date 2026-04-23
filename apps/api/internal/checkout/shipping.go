package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// shippingContext bundles everything we need to build a quote request for a cart.
type shippingContext struct {
	CartID            string
	StoreID           string
	EventFreeShipping bool

	OriginZip          string
	DefaultPkgWeightG  int
	DefaultPkgFormat   string

	Items []shippingContextItem
}

type shippingContextItem struct {
	ProductID           string
	SKU                 string
	Keyword             string
	Name                string
	Quantity            int
	UnitPriceCents      int64
	WeightGrams         int
	HeightCm            int
	WidthCm             int
	LengthCm            int
	PackageFormat       string
	InsuranceValueCents int64
}

// GetShippingContextByToken loads the cart, its store origin, event free_shipping
// flag and all cart items with their shipping dimensions. Errors if the cart
// has any item missing dimensions (cannot quote).
func (r *Repository) GetShippingContextByToken(ctx context.Context, pool *pgxpool.Pool, token string) (*shippingContext, error) {
	const q = `
		SELECT
			c.id::text,
			s.id::text,
			COALESCE(e.free_shipping, false),
			COALESCE(s.address_zip, ''),
			COALESCE(s.default_package_weight_grams, 0),
			COALESCE(s.default_package_format, 'box')
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		JOIN stores s      ON s.id = e.store_id
		WHERE c.token = $1
	`
	var ctxOut shippingContext
	err := pool.QueryRow(ctx, q, token).Scan(
		&ctxOut.CartID,
		&ctxOut.StoreID,
		&ctxOut.EventFreeShipping,
		&ctxOut.OriginZip,
		&ctxOut.DefaultPkgWeightG,
		&ctxOut.DefaultPkgFormat,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("carrinho não encontrado")
		}
		return nil, fmt.Errorf("loading shipping context: %w", err)
	}

	const itemsQ = `
		SELECT
			ci.product_id::text,
			COALESCE(p.sku, ''),
			COALESCE(p.keyword, ''),
			p.name,
			(ci.quantity - COALESCE(ci.waitlisted_quantity, 0)) AS available_qty,
			COALESCE(ci.unit_price, 0),
			COALESCE(p.weight_grams, 0),
			COALESCE(p.height_cm, 0),
			COALESCE(p.width_cm, 0),
			COALESCE(p.length_cm, 0),
			COALESCE(p.package_format, 'box'),
			COALESCE(p.insurance_value_cents, p.price, 0)
		FROM cart_items ci
		JOIN products p ON p.id = ci.product_id
		WHERE ci.cart_id = $1
		ORDER BY ci.id
	`
	cartUID, err := uuid.Parse(ctxOut.CartID)
	if err != nil {
		return nil, fmt.Errorf("invalid cart id: %w", err)
	}
	rows, err := pool.Query(ctx, itemsQ, pgtype.UUID{Bytes: cartUID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("loading cart items for shipping: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var it shippingContextItem
		if err := rows.Scan(
			&it.ProductID, &it.SKU, &it.Keyword, &it.Name,
			&it.Quantity, &it.UnitPriceCents,
			&it.WeightGrams, &it.HeightCm, &it.WidthCm, &it.LengthCm,
			&it.PackageFormat, &it.InsuranceValueCents,
		); err != nil {
			return nil, fmt.Errorf("scanning cart item: %w", err)
		}
		if it.Quantity <= 0 {
			continue
		}
		ctxOut.Items = append(ctxOut.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating cart items: %w", err)
	}
	return &ctxOut, nil
}

// UpdateCartShipping persists the freight option chosen by the customer.
// When clear is true, the shipping fields are set to NULL (unselect).
func (r *Repository) UpdateCartShipping(ctx context.Context, pool *pgxpool.Pool, cartID string, sel *CartShippingSelection) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}

	if sel == nil {
		_, err = pool.Exec(ctx, `
			UPDATE carts
			SET shipping_service_id = NULL,
			    shipping_service_name = NULL,
			    shipping_carrier = NULL,
			    shipping_cost_cents = NULL,
			    shipping_cost_real_cents = NULL,
			    shipping_deadline_days = NULL,
			    shipping_quoted_at = NULL
			WHERE id = $1
		`, pgtype.UUID{Bytes: uid, Valid: true})
		if err != nil {
			return fmt.Errorf("clearing cart shipping: %w", err)
		}
		return nil
	}

	_, err = pool.Exec(ctx, `
		UPDATE carts
		SET shipping_service_id      = $2,
		    shipping_service_name    = $3,
		    shipping_carrier         = $4,
		    shipping_cost_cents      = $5,
		    shipping_cost_real_cents = $6,
		    shipping_deadline_days   = $7,
		    shipping_quoted_at       = now()
		WHERE id = $1
	`,
		pgtype.UUID{Bytes: uid, Valid: true},
		sel.ServiceID,
		sel.ServiceName,
		sel.Carrier,
		sel.CostCents,
		sel.RealCostCents,
		sel.DeadlineDays,
	)
	if err != nil {
		return fmt.Errorf("updating cart shipping: %w", err)
	}
	return nil
}

// ReadCartShipping fetches the freight option currently stored on the cart, if any.
func (r *Repository) ReadCartShipping(ctx context.Context, pool *pgxpool.Pool, cartID string) (*CartShippingSelection, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid cart ID")
	}
	var (
		serviceID    pgtype.Int4
		serviceName  pgtype.Text
		carrier      pgtype.Text
		cost         pgtype.Int8
		realCost     pgtype.Int8
		deadlineDays pgtype.Int4
		freeShipping bool
	)
	err = pool.QueryRow(ctx, `
		SELECT c.shipping_service_id,
		       c.shipping_service_name,
		       c.shipping_carrier,
		       c.shipping_cost_cents,
		       c.shipping_cost_real_cents,
		       c.shipping_deadline_days,
		       COALESCE(e.free_shipping, false)
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE c.id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}).Scan(
		&serviceID, &serviceName, &carrier, &cost, &realCost, &deadlineDays, &freeShipping,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading cart shipping: %w", err)
	}
	if !serviceID.Valid {
		return nil, nil
	}
	return &CartShippingSelection{
		ServiceID:     int(serviceID.Int32),
		ServiceName:   serviceName.String,
		Carrier:       carrier.String,
		CostCents:     cost.Int64,
		RealCostCents: realCost.Int64,
		DeadlineDays:  int(deadlineDays.Int32),
		FreeShipping:  freeShipping,
	}, nil
}

// =============================================================================
// SERVICE METHODS
// =============================================================================

// QuoteShipping calls the store's shipping provider to get freight options for
// the cart's items. Does not persist anything.
func (s *Service) QuoteShipping(ctx context.Context, input QuoteShippingInput) (*QuoteShippingOutput, error) {
	shipCtx, err := s.repo.GetShippingContextByToken(ctx, s.pool, input.Token)
	if err != nil {
		return nil, err
	}
	if shipCtx.OriginZip == "" {
		return nil, httpx.ErrUnprocessable("loja sem CEP de origem cadastrado")
	}
	if len(shipCtx.Items) == 0 {
		return nil, httpx.ErrUnprocessable("nenhum item no carrinho para cotar")
	}

	items := make([]providers.ShippingItem, 0, len(shipCtx.Items))
	for _, it := range shipCtx.Items {
		if it.WeightGrams <= 0 || it.HeightCm <= 0 || it.WidthCm <= 0 || it.LengthCm <= 0 {
			return nil, httpx.ErrUnprocessable(fmt.Sprintf("produto %q sem dimensões cadastradas", it.Name))
		}
		id := it.SKU
		if id == "" {
			id = it.Keyword
		}
		if id == "" {
			id = it.ProductID
		}
		items = append(items, providers.ShippingItem{
			ID:                  id,
			WeightGrams:         it.WeightGrams,
			HeightCm:            it.HeightCm,
			WidthCm:             it.WidthCm,
			LengthCm:            it.LengthCm,
			InsuranceValueCents: it.InsuranceValueCents,
			Quantity:            it.Quantity,
			PackageFormat:       it.PackageFormat,
		})
	}

	provider, err := s.integrationService.GetShippingProvider(ctx, shipCtx.StoreID)
	if err != nil {
		return nil, err
	}

	options, err := provider.Quote(ctx, providers.QuoteRequest{
		FromZip:                 providers.ShippingZip(shipCtx.OriginZip),
		ToZip:                   providers.ShippingZip(input.ZipCode),
		Items:                   items,
		ExtraPackageWeightGrams: shipCtx.DefaultPkgWeightG,
		ServiceIDs:              input.ServiceIDs,
	})
	if err != nil {
		s.logger.Error("shipping quote failed",
			zap.String("store_id", shipCtx.StoreID),
			zap.String("cart_token", input.Token),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("falha ao cotar frete: " + err.Error())
	}

	out := &QuoteShippingOutput{
		QuotedAt:     time.Now(),
		FreeShipping: shipCtx.EventFreeShipping,
		Options:      make([]ShippingQuoteOptionResponse, 0, len(options)),
	}
	for _, opt := range options {
		resp := ShippingQuoteOptionResponse{
			ID:             opt.ServiceID,
			Service:        opt.Service,
			Carrier:        opt.Carrier,
			CarrierLogoURL: opt.CarrierLogo,
			RealPriceCents: opt.PriceCents,
			DeadlineDays:   opt.DeadlineDays,
			Available:      opt.Available,
			Error:          opt.Error,
		}
		if shipCtx.EventFreeShipping {
			resp.PriceCents = 0
		} else {
			resp.PriceCents = opt.PriceCents
		}
		out.Options = append(out.Options, resp)
	}
	return out, nil
}

// SelectShippingMethod re-quotes to make sure the chosen service is still valid
// and persists the selection on the cart. Re-quoting is cheap and prevents the
// customer from locking in a stale price.
func (s *Service) SelectShippingMethod(ctx context.Context, input SelectShippingMethodInput) (*SelectShippingMethodOutput, error) {
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrUnprocessable("carrinho já foi pago")
	}
	if input.ZipCode == "" {
		return nil, httpx.ErrUnprocessable("CEP é obrigatório para confirmar o frete")
	}

	quote, err := s.QuoteShipping(ctx, QuoteShippingInput{
		Token:      input.Token,
		ZipCode:    input.ZipCode,
		ServiceIDs: []int{input.ServiceID},
	})
	if err != nil {
		return nil, err
	}

	var chosen *ShippingQuoteOptionResponse
	for i := range quote.Options {
		if quote.Options[i].ID == input.ServiceID && quote.Options[i].Available {
			chosen = &quote.Options[i]
			break
		}
	}
	if chosen == nil {
		return nil, httpx.ErrUnprocessable("opção de frete indisponível")
	}

	sel := &CartShippingSelection{
		ServiceID:     chosen.ID,
		ServiceName:   chosen.Service,
		Carrier:       chosen.Carrier,
		CostCents:     chosen.PriceCents,
		RealCostCents: chosen.RealPriceCents,
		DeadlineDays:  chosen.DeadlineDays,
		FreeShipping:  quote.FreeShipping,
	}
	if err := s.repo.UpdateCartShipping(ctx, s.pool, cart.ID, sel); err != nil {
		return nil, err
	}

	// Recompute summary on the fly.
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}
	summary := buildSummary(items, sel)

	return &SelectShippingMethodOutput{
		Shipping: *sel,
		Summary:  summary,
	}, nil
}

// buildSummary computes the cart summary from the items and a shipping selection.
func buildSummary(items []CartItemRow, sel *CartShippingSelection) CartSummary {
	var subtotal int64
	var totalItems int
	for _, it := range items {
		available := it.Quantity - it.WaitlistedQuantity
		if available > 0 {
			subtotal += it.UnitPrice * int64(available)
			totalItems += available
		}
	}
	summary := CartSummary{
		Subtotal:   subtotal,
		TotalItems: totalItems,
		Total:      subtotal,
	}
	if sel != nil {
		summary.ShippingCost = sel.CostCents
		summary.Total = subtotal + sel.CostCents
		summary.HasShippingQuote = true
	}
	return summary
}

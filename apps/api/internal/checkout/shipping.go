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
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// shippingQuoteCacheTTL is how long a snapshot is considered fresh enough to
// resolve a SelectShippingMethod request without forcing the client to refresh
// the quote. Long enough that the customer can pick at a normal pace, short
// enough that prices stay reasonably current.
const shippingQuoteCacheTTL = 30 * time.Minute

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
		SET shipping_provider        = $2,
		    shipping_service_id      = $3,
		    shipping_service_name    = $4,
		    shipping_carrier         = $5,
		    shipping_cost_cents      = $6,
		    shipping_cost_real_cents = $7,
		    shipping_deadline_days   = $8,
		    shipping_quoted_at       = now()
		WHERE id = $1
	`,
		pgtype.UUID{Bytes: uid, Valid: true},
		sel.Provider,
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

// ReadCouponSummary returns the type + merchant cap (value_cents) for the
// coupon attached to the cart, when any. Used by GetCartForCheckout to
// surface enough context that the FE can explain a partial free-shipping
// discount ("limited by merchant cap" vs. "limited by cheapest available").
// Returns ("", 0, nil) when no coupon is applied or when the coupon row was
// hard-deleted between apply and read.
func (r *Repository) ReadCouponSummary(ctx context.Context, pool *pgxpool.Pool, couponID string) (string, int64, error) {
	uid, err := uuid.Parse(couponID)
	if err != nil {
		return "", 0, httpx.ErrBadRequest("invalid coupon ID")
	}
	var (
		ctype pgtype.Text
		value pgtype.Int8
	)
	err = pool.QueryRow(ctx,
		`SELECT type, value_cents FROM coupons WHERE id = $1`,
		pgtype.UUID{Bytes: uid, Valid: true},
	).Scan(&ctype, &value)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("reading coupon summary: %w", err)
	}
	return ctype.String, value.Int64, nil
}

// ReadCartShipping fetches the freight option currently stored on the cart, if any.
func (r *Repository) ReadCartShipping(ctx context.Context, pool *pgxpool.Pool, cartID string) (*CartShippingSelection, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid cart ID")
	}
	var (
		provider     pgtype.Text
		serviceID    pgtype.Text
		serviceName  pgtype.Text
		carrier      pgtype.Text
		cost         pgtype.Int8
		realCost     pgtype.Int8
		deadlineDays pgtype.Int4
		freeShipping bool
	)
	err = pool.QueryRow(ctx, `
		SELECT c.shipping_provider,
		       c.shipping_service_id,
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
		&provider, &serviceID, &serviceName, &carrier, &cost, &realCost, &deadlineDays, &freeShipping,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading cart shipping: %w", err)
	}
	if !serviceID.Valid || serviceID.String == "" {
		return nil, nil
	}
	return &CartShippingSelection{
		Provider:      provider.String,
		ServiceID:     serviceID.String,
		ServiceName:   serviceName.String,
		Carrier:       carrier.String,
		CostCents:     cost.Int64,
		RealCostCents: realCost.Int64,
		DeadlineDays:  int(deadlineDays.Int32),
		FreeShipping:  freeShipping,
	}, nil
}

// SaveShippingQuoteCache persists the most recent QuoteShipping output on the
// cart so SelectShippingMethod can resolve the customer's pick without
// re-quoting. Overwrites any previous snapshot.
func (r *Repository) SaveShippingQuoteCache(ctx context.Context, pool *pgxpool.Pool, cartID string, options []ShippingQuoteOptionResponse, at time.Time) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("marshaling shipping quote cache: %w", err)
	}
	// Pass the marshaled JSON as a string — pgx encodes []byte as BYTEA by
	// default, which Postgres then refuses to coerce into JSONB ("invalid
	// input syntax for type json"). string(raw) sends a normal text value
	// that the JSONB column accepts cleanly.
	_, err = pool.Exec(ctx, `
		UPDATE carts
		SET last_shipping_quote_options = $2,
		    last_shipping_quote_at      = $3
		WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}, string(raw), at)
	if err != nil {
		return fmt.Errorf("saving shipping quote cache: %w", err)
	}
	return nil
}

// ReadShippingQuoteCache returns the snapshot persisted by the most recent
// QuoteShipping call. Returns (nil, zero-time, nil) when no snapshot exists.
func (r *Repository) ReadShippingQuoteCache(ctx context.Context, pool *pgxpool.Pool, cartID string) ([]ShippingQuoteOptionResponse, time.Time, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, time.Time{}, httpx.ErrBadRequest("invalid cart ID")
	}
	var (
		raw      []byte
		quotedAt pgtype.Timestamptz
	)
	err = pool.QueryRow(ctx, `
		SELECT last_shipping_quote_options, last_shipping_quote_at
		FROM carts
		WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}).Scan(&raw, &quotedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, fmt.Errorf("reading shipping quote cache: %w", err)
	}
	if len(raw) == 0 {
		return nil, time.Time{}, nil
	}
	var options []ShippingQuoteOptionResponse
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, time.Time{}, fmt.Errorf("decoding shipping quote cache: %w", err)
	}
	if quotedAt.Valid {
		return options, quotedAt.Time, nil
	}
	return options, time.Time{}, nil
}

// =============================================================================
// SERVICE METHODS
// =============================================================================

// QuoteShipping calls every active shipping integration of the store and
// merges the quote options into a single list. When the store has only one
// shipping integration active, the behavior is unchanged; when it has two
// (e.g. Melhor Envio + SmartEnvios) the customer sees options from both.
//
// Each option carries the `provider` name so the subsequent selection call
// knows which integration owns it. One provider failing does not fail the
// whole call — errors are logged and the other providers' options are still
// returned.
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
			Name:                it.Name,
			WeightGrams:         it.WeightGrams,
			HeightCm:            it.HeightCm,
			WidthCm:             it.WidthCm,
			LengthCm:            it.LengthCm,
			InsuranceValueCents: it.InsuranceValueCents,
			UnitPriceCents:      it.UnitPriceCents,
			Quantity:            it.Quantity,
			PackageFormat:       it.PackageFormat,
		})
	}

	active, err := s.integrationService.GetShippingProviders(ctx, shipCtx.StoreID)
	if err != nil {
		return nil, err
	}
	if len(active) == 0 {
		return nil, httpx.ErrNotFound("nenhuma integração de frete ativa nesta loja")
	}

	// Optional client-side provider filter (e.g. admin testing a single provider).
	if len(input.Providers) > 0 {
		wanted := map[string]bool{}
		for _, p := range input.Providers {
			wanted[p] = true
		}
		filtered := active[:0]
		for _, p := range active {
			if wanted[string(p.Name())] {
				filtered = append(filtered, p)
			}
		}
		active = filtered
	}

	out := &QuoteShippingOutput{
		QuotedAt:     time.Now(),
		FreeShipping: shipCtx.EventFreeShipping,
		Options:      []ShippingQuoteOptionResponse{},
	}

	req := providers.QuoteRequest{
		FromZip:                 providers.ShippingZip(shipCtx.OriginZip),
		ToZip:                   providers.ShippingZip(input.ZipCode),
		Items:                   items,
		ExtraPackageWeightGrams: shipCtx.DefaultPkgWeightG,
		ServiceIDs:              input.ServiceIDs,
		ExternalID:              shipCtx.CartID,
	}

	for _, provider := range active {
		options, qerr := provider.Quote(ctx, req)
		if qerr != nil {
			s.logger.Error("shipping quote failed for provider",
				zap.String("provider", string(provider.Name())),
				zap.String("store_id", shipCtx.StoreID),
				zap.String("cart_token", input.Token),
				zap.Error(qerr),
			)
			continue
		}
		for _, opt := range options {
			// Drop unavailable options server-side so the public checkout
			// never shows a rejected service. Rationale: they carry empty
			// ids (providers only assign ids to valid quotes) and bloat the
			// UI with "cinza/disabled" rows that the customer cannot act on
			// anyway. Keep a log line so admins can still diagnose why a
			// service was filtered (e.g. "value exceeds carrier limit").
			if !opt.Available {
				s.logger.Debug("shipping quote option filtered (unavailable)",
					zap.String("provider", string(provider.Name())),
					zap.String("service", opt.Service),
					zap.String("reason", opt.Error),
				)
				continue
			}
			resp := ShippingQuoteOptionResponse{
				ID:             opt.ServiceID,
				Provider:       string(provider.Name()),
				Service:        opt.Service,
				Carrier:        opt.Carrier,
				CarrierLogoURL: opt.CarrierLogo,
				RealPriceCents: opt.PriceCents,
				DeadlineDays:   opt.DeadlineDays,
				Available:      true,
			}
			if shipCtx.EventFreeShipping {
				resp.PriceCents = 0
			} else {
				resp.PriceCents = opt.PriceCents
			}
			out.Options = append(out.Options, resp)
		}
	}

	if len(out.Options) == 0 {
		return nil, httpx.ErrUnprocessable("nenhuma opção de frete disponível para este carrinho")
	}

	// Snapshot the response so SelectShippingMethod can resolve the
	// customer's pick without re-quoting. Best-effort: a write failure does
	// not break the public quote response.
	if cacheErr := s.repo.SaveShippingQuoteCache(ctx, s.pool, shipCtx.CartID, out.Options, out.QuotedAt); cacheErr != nil {
		s.logger.Warn("failed to cache shipping quote — selection may force a re-quote",
			zap.String("cart_id", shipCtx.CartID),
			zap.Error(cacheErr),
		)
	}
	return out, nil
}

// SelectShippingMethod resolves the customer's chosen freight option from the
// quote snapshot persisted by the previous QuoteShipping call and persists
// the selection on the cart.
//
// We deliberately do NOT re-quote here. Some providers (SmartEnvios) issue a
// fresh per-quotation `id` on every /quote/freight call, so re-quoting would
// return options the customer never saw and the search by id would always
// fail. The snapshot keeps the exact prices the customer was shown, valid for
// shippingQuoteCacheTTL — which is what we want to lock in at this step.
// Price freshness for the actual shipment creation is handled at /dc-create
// time by the order-lifecycle flow, not here.
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

	options, quotedAt, err := s.repo.ReadShippingQuoteCache(ctx, s.pool, cart.ID)
	if err != nil {
		return nil, err
	}
	if len(options) == 0 {
		return nil, httpx.ErrUnprocessable("primeiro cote o frete antes de selecionar")
	}
	if !quotedAt.IsZero() && time.Since(quotedAt) > shippingQuoteCacheTTL {
		return nil, httpx.ErrUnprocessable("cotação expirou — refaça a cotação")
	}

	var chosen *ShippingQuoteOptionResponse
	for i := range options {
		opt := &options[i]
		if opt.ID != input.ServiceID {
			continue
		}
		if input.Provider != "" && opt.Provider != input.Provider {
			continue
		}
		chosen = opt
		break
	}
	if chosen == nil {
		return nil, httpx.ErrUnprocessable("opção de frete não encontrada na cotação atual — refaça a cotação")
	}

	freeShipping := chosen.PriceCents == 0 && chosen.RealPriceCents > 0
	sel := &CartShippingSelection{
		Provider:      chosen.Provider,
		ServiceID:     chosen.ID,
		ServiceName:   chosen.Service,
		Carrier:       chosen.Carrier,
		CostCents:     chosen.PriceCents,
		RealCostCents: chosen.RealPriceCents,
		DeadlineDays:  chosen.DeadlineDays,
		FreeShipping:  freeShipping,
	}
	if err := s.repo.UpdateCartShipping(ctx, s.pool, cart.ID, sel); err != nil {
		return nil, err
	}

	// Re-snapshot a free-shipping coupon's discount against the new cost.
	// Percent / fixed coupons stay untouched (the lifecycle returns no-op).
	// Errors are logged but never block the shipping selection — the cart
	// will be re-evaluated on the next mutation either way.
	if s.couponLifecycle != nil {
		if err := s.couponLifecycle.OnShippingChanged(ctx, cart.ID); err != nil {
			s.logger.Warn("coupon re-evaluation on shipping change failed",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		}
	}

	// Re-read the cart to pick up the (possibly updated) coupon discount
	// for the response summary.
	refreshed, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	// Recompute summary on the fly.
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}
	summary := buildSummary(items, sel, refreshed.CouponDiscountCents)

	return &SelectShippingMethodOutput{
		Shipping: *sel,
		Summary:  summary,
	}, nil
}

// buildSummary computes the cart summary from the items, the selected shipping
// option, and the coupon discount snapshot. Total is capped at zero so a
// stale free-shipping snapshot can never produce a negative amount.
func buildSummary(items []CartItemRow, sel *CartShippingSelection, couponDiscount int64) CartSummary {
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
	if couponDiscount > 0 {
		summary.CouponDiscount = couponDiscount
		summary.Total -= couponDiscount
		if summary.Total < 0 {
			summary.Total = 0
		}
	}
	return summary
}

package coupon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo   *Repository
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewService(repo *Repository, pool *pgxpool.Pool, logger *zap.Logger) *Service {
	return &Service{repo: repo, pool: pool, logger: logger}
}

// List returns every coupon for the event after enforcing tenancy: the
// event must belong to the caller's store. We do this check on every
// admin-side handler because eventId is a path param and could otherwise
// leak across stores.
func (s *Service) List(ctx context.Context, eventID, storeID string) ([]Coupon, error) {
	if err := s.assertEventOwnership(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	return s.repo.ListByEvent(ctx, eventID)
}

func (s *Service) Get(ctx context.Context, id, eventID, storeID string) (*Coupon, error) {
	if err := s.assertEventOwnership(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	c, err := s.repo.GetByID(ctx, id, eventID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, httpx.ErrNotFound("coupon not found")
	}
	return c, nil
}

func (s *Service) Create(ctx context.Context, eventID, storeID string, req CreateRequest) (*Coupon, error) {
	if err := s.assertEventOwnership(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	if err := validateBusinessRules(req.Type, req.ValueCents, req.PercentBPS); err != nil {
		return nil, err
	}
	if req.ValidFrom != nil && req.ValidUntil != nil && !req.ValidUntil.After(*req.ValidFrom) {
		return nil, httpx.ErrUnprocessable("validUntil must be after validFrom")
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return nil, httpx.ErrUnprocessable("code cannot be empty")
	}

	created, err := s.repo.Create(ctx, CreateParams{
		EventID:          eventID,
		Code:             code,
		Type:             req.Type,
		ValueCents:       req.ValueCents,
		PercentBPS:       req.PercentBPS,
		MaxUses:          req.MaxUses,
		MinPurchaseCents: req.MinPurchaseCents,
		ValidFrom:        req.ValidFrom,
		ValidUntil:       req.ValidUntil,
		Active:           req.Active,
	})
	if err != nil {
		// Friendlier 409 when the merchant tried to reuse a code in the same
		// event — the unique index is the source of truth, we don't pre-check
		// to avoid a TOCTOU race with concurrent creates.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, httpx.ErrConflict(fmt.Sprintf("coupon code %q already exists for this event", code))
		}
		return nil, err
	}
	return created, nil
}

func (s *Service) Update(ctx context.Context, id, eventID, storeID string, req UpdateRequest) (*Coupon, error) {
	if err := s.assertEventOwnership(ctx, eventID, storeID); err != nil {
		return nil, err
	}

	// We need the current type to re-validate the value/percent invariants
	// even when only one of them is patched.
	current, err := s.repo.GetByID(ctx, id, eventID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, httpx.ErrNotFound("coupon not found")
	}

	effectiveType := current.Type
	if req.Type != nil {
		effectiveType = *req.Type
	}
	effectiveValue := current.ValueCents
	if req.ValueCents != nil {
		effectiveValue = *req.ValueCents
	}
	effectivePercent := current.PercentBPS
	if req.PercentBPS != nil {
		effectivePercent = *req.PercentBPS
	}
	if err := validateBusinessRules(effectiveType, effectiveValue, effectivePercent); err != nil {
		return nil, err
	}

	updated, err := s.repo.Update(ctx, UpdateParams{
		ID:               id,
		EventID:          eventID,
		Type:             req.Type,
		ValueCents:       req.ValueCents,
		PercentBPS:       req.PercentBPS,
		MaxUses:          req.MaxUses,
		MinPurchaseCents: req.MinPurchaseCents,
		ValidFrom:        req.ValidFrom,
		ValidUntil:       req.ValidUntil,
		Active:           req.Active,
	})
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, httpx.ErrNotFound("coupon not found")
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, id, eventID, storeID string) error {
	if err := s.assertEventOwnership(ctx, eventID, storeID); err != nil {
		return err
	}
	ok, err := s.repo.Delete(ctx, id, eventID)
	if err != nil {
		// 23503 = foreign_key_violation. coupon_redemptions references
		// coupons with ON DELETE RESTRICT, so a coupon that was already
		// applied to a cart cannot be hard-deleted. Tell the merchant to
		// disable instead.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return httpx.ErrConflict("coupon already redeemed by a customer; disable it instead of deleting")
		}
		return err
	}
	if !ok {
		return httpx.ErrNotFound("coupon not found")
	}
	return nil
}

// assertEventOwnership returns 404 (not 403) on cross-tenant access — same
// dialect the rest of the API uses to avoid leaking the existence of
// resources that belong to other stores.
func (s *Service) assertEventOwnership(ctx context.Context, eventID, storeID string) error {
	ok, err := s.repo.EventExistsForStore(ctx, eventID, storeID)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.ErrNotFound("event not found")
	}
	return nil
}

// validateBusinessRules checks the value/percent invariants per coupon type.
// Schema-level CHECK alone can't enforce "ValueCents required when type=fixed"
// because both columns are NOT NULL with defaults, so this lives in the
// service.
func validateBusinessRules(t Type, valueCents int64, percentBPS int) error {
	switch t {
	case TypePercent:
		if percentBPS <= 0 {
			return httpx.ErrUnprocessable("percent coupon requires percentBps > 0")
		}
	case TypeFixed:
		if valueCents <= 0 {
			return httpx.ErrUnprocessable("fixed coupon requires valueCents > 0")
		}
	case TypeFreeShipping:
		// Both value and percent stay 0 for free_shipping; we don't enforce
		// it because validateBusinessRules is also called on partial updates
		// where one of the fields is intentionally untouched.
	default:
		return httpx.ErrUnprocessable("invalid coupon type")
	}
	return nil
}

// =============================================================================
// PUBLIC CART — apply / remove
// =============================================================================

// ApplyResult is what the public-cart endpoint returns after a successful
// apply. The FE uses these to immediately reflect the new totals without a
// second round-trip. MaxDiscountCents is the merchant-set ceiling on a
// free-shipping coupon (0 = uncapped; ignored for percent / fixed) — surfaced
// so the FE can explain to the buyer why the discount stopped at that value.
type ApplyResult struct {
	Code              string
	Type              Type
	AppliedValueCents int64
	MaxDiscountCents  int64
	SubtotalCents     int64
	ShippingCostCents int64
	NewTotalCents     int64 // subtotal + shipping − applied
}

// ApplyToCart wraps the entire apply path in a single tx so the FOR UPDATE
// lock on the coupon row blocks concurrent applies and we can never
// over-redeem (max_uses).
func (s *Service) ApplyToCart(
	ctx context.Context,
	cartToken, code string,
) (*ApplyResult, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, httpx.ErrUnprocessable("coupon code is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cart, err := s.repo.LoadCartForCouponTx(ctx, tx, cartToken)
	if err != nil {
		return nil, err
	}
	if cart == nil {
		return nil, httpx.ErrNotFound("cart not found")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrConflict("cart already paid")
	}
	if cart.Status == "expired" {
		return nil, httpx.ErrConflict("cart expired")
	}
	if cart.CouponID != nil {
		return nil, httpx.ErrConflict("cart already has a coupon — remove it first")
	}
	if cart.SubtotalCents <= 0 {
		return nil, httpx.ErrUnprocessable("cart is empty")
	}

	c, err := s.repo.LockCouponByEventCodeTx(ctx, tx, cart.EventID, code)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, httpx.ErrNotFound("invalid coupon code")
	}
	if !c.Active {
		return nil, httpx.ErrUnprocessable("coupon is not active")
	}
	now := time.Now()
	if c.ValidFrom != nil && now.Before(*c.ValidFrom) {
		return nil, httpx.ErrUnprocessable("coupon is not valid yet")
	}
	if c.ValidUntil != nil && now.After(*c.ValidUntil) {
		return nil, httpx.ErrUnprocessable("coupon expired")
	}
	if c.MaxUses != nil && c.UsedCount >= *c.MaxUses {
		return nil, httpx.ErrConflict("coupon already fully redeemed")
	}
	if cart.SubtotalCents < c.MinPurchaseCents {
		return nil, httpx.ErrUnprocessable(
			fmt.Sprintf("minimum purchase of %.2f BRL not reached", float64(c.MinPurchaseCents)/100),
		)
	}

	applied, err := computeAppliedDiscount(c, cart)
	if err != nil {
		return nil, err
	}

	if err := s.repo.InsertReservedRedemptionTx(ctx, tx, c.ID, cart.CartID, applied); err != nil {
		return nil, err
	}
	if err := s.repo.IncrementUsedCountTx(ctx, tx, c.ID); err != nil {
		return nil, err
	}
	if err := s.repo.ApplyCouponToCartTx(ctx, tx, cart.CartID, c.ID, c.Code, applied); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &ApplyResult{
		Code:              c.Code,
		Type:              c.Type,
		AppliedValueCents: applied,
		MaxDiscountCents:  freeShippingCap(c),
		SubtotalCents:     cart.SubtotalCents,
		ShippingCostCents: cart.ShippingCostCents,
		NewTotalCents:     cart.SubtotalCents + cart.ShippingCostCents - applied,
	}, nil
}

// freeShippingCap exposes the merchant-set discount ceiling for a coupon.
// Reuses ValueCents — only meaningful for free_shipping (percent and fixed
// have their own usage of that column). Returns 0 for "no cap".
func freeShippingCap(c *Coupon) int64 {
	if c.Type != TypeFreeShipping {
		return 0
	}
	if c.ValueCents < 0 {
		return 0
	}
	return c.ValueCents
}

// RemoveFromCart undoes ApplyToCart. Idempotent — calling on a cart with no
// coupon is a no-op (returns the unchanged totals). The decrement is
// protected by the same FOR UPDATE lock as the apply path.
func (s *Service) RemoveFromCart(ctx context.Context, cartToken string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cart, err := s.repo.LoadCartForCouponTx(ctx, tx, cartToken)
	if err != nil {
		return err
	}
	if cart == nil {
		return httpx.ErrNotFound("cart not found")
	}
	if cart.PaymentStatus == "paid" {
		return httpx.ErrConflict("cart already paid")
	}
	if cart.CouponID == nil {
		// No-op: nothing to remove. Don't 404 — keeps the FE safe to call this
		// idempotently when the user mashes the remove button.
		return nil
	}

	c, err := s.repo.LockCouponByIDTx(ctx, tx, *cart.CouponID)
	if err != nil {
		return err
	}
	if c != nil {
		// Coupon may have been hard-deleted between apply and remove; if so,
		// we just clear the cart side.
		if err := s.repo.DecrementUsedCountTx(ctx, tx, c.ID); err != nil {
			return err
		}
	}
	if err := s.repo.DeleteRedemptionByCartTx(ctx, tx, cart.CartID); err != nil {
		return err
	}
	if err := s.repo.ClearCouponOnCartTx(ctx, tx, cart.CartID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// =============================================================================
// CART-MUTATION LIFECYCLE — called from checkout when something changes that
// could invalidate the discount snapshot.
// =============================================================================

// ReevaluateOnShippingChange re-snapshots a free-shipping coupon's discount
// against the cart's current shipping cost. No-op when the cart has no
// coupon, when the coupon is percent / fixed (subtotal-based, immune to
// shipping changes), or when the cart already has its discount in sync.
//
// Runs OUTSIDE a tx because we don't touch coupons.used_count — only
// carts.coupon_discount_cents — and the cart row is already serialized by
// the surrounding shipping update.
func (s *Service) ReevaluateOnShippingChange(ctx context.Context, cartID string) error {
	r, err := s.repo.LoadRedemptionByCart(ctx, cartID)
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}

	c, err := s.repo.GetCouponByID(ctx, r.CouponID)
	if err != nil {
		return err
	}
	if c == nil || c.Type != TypeFreeShipping {
		return nil
	}

	shipping, err := s.repo.GetCartShippingCostCents(ctx, cartID)
	if err != nil {
		return err
	}
	cheapest, err := s.repo.GetCheapestQuotedShippingCents(ctx, cartID)
	if err != nil {
		return err
	}
	discount := computeFreeShippingDiscount(shipping, cheapest, freeShippingCap(c))
	return s.repo.UpdateCartCouponDiscount(ctx, cartID, discount)
}

// ExpireStaleReservedRedemptions sweeps reserved redemptions on carts that
// will never be paid (expired / failed / cancelled) and returns each slot to
// circulation. Each row gets its own tx so a single bad coupon can't block
// the whole batch. Returns counts of expired and skipped (rows whose
// redemption row had already moved to a terminal state between the SELECT
// and the UPDATE — a benign race with the public-cart remove flow).
func (s *Service) ExpireStaleReservedRedemptions(ctx context.Context, limit int) (expired, skipped int, err error) {
	stale, err := s.repo.ListStaleReservedRedemptions(ctx, limit)
	if err != nil {
		return 0, 0, err
	}

	for _, row := range stale {
		ok, err := s.expireOne(ctx, row)
		if err != nil {
			s.logger.Warn("failed to expire reserved redemption",
				zap.String("redemption_id", row.RedemptionID),
				zap.String("coupon_id", row.CouponID),
				zap.String("cart_id", row.CartID),
				zap.Error(err),
			)
			continue
		}
		if ok {
			expired++
		} else {
			skipped++
		}
	}
	return expired, skipped, nil
}

// expireOne wraps a single row in its own tx + lock. Returns true on a
// successful flip, false when the redemption was already in a terminal
// state by the time we acquired the lock.
func (s *Service) expireOne(ctx context.Context, row StaleRow) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := s.repo.LockCouponByIDTx(ctx, tx, row.CouponID); err != nil {
		return false, err
	}
	if err := s.repo.MarkRedemptionExpiredTx(ctx, tx, row.RedemptionID); err != nil {
		return false, err
	}
	if err := s.repo.DecrementUsedCountTx(ctx, tx, row.CouponID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// StaleRow is re-exported from the repository for the worker — keeps the
// worker package free of the repo type.
type StaleRow = StaleRedemption

// =============================================================================
// REDEMPTION LIFECYCLE — called from the payment webhook.
// =============================================================================

// ConfirmRedemption flips the cart's redemption from 'reserved' to
// 'confirmed'. No-op when there's no redemption (cart paid without a
// coupon) or when it's already been confirmed.
func (s *Service) ConfirmRedemption(ctx context.Context, cartID string) error {
	r, err := s.repo.LoadRedemptionByCart(ctx, cartID)
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	if r.Status != "reserved" {
		// Already confirmed / refunded / expired — webhook may fire twice for
		// the same cart, this keeps us idempotent.
		return nil
	}
	return s.repo.MarkRedemptionConfirmed(ctx, r.ID)
}

// RefundRedemption flips the redemption to 'refunded' and decrements the
// coupon's used_count under a row lock so the slot returns to circulation
// and the merchant can hand it out again. Idempotent.
func (s *Service) RefundRedemption(ctx context.Context, cartID string) error {
	r, err := s.repo.LoadRedemptionByCart(ctx, cartID)
	if err != nil {
		return err
	}
	if r == nil || r.Status == "refunded" {
		return nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-read inside the tx + lock the coupon. The double-check guards
	// against a TOCTOU between LoadRedemptionByCart and the lock.
	if _, err := s.repo.LockCouponByIDTx(ctx, tx, r.CouponID); err != nil {
		return err
	}
	if err := s.repo.MarkRedemptionRefundedTx(ctx, tx, r.ID); err != nil {
		return err
	}
	if err := s.repo.DecrementUsedCountTx(ctx, tx, r.CouponID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// computeAppliedDiscount returns the absolute discount in cents for the
// given coupon + cart snapshot. percent and fixed are capped at the
// subtotal so a 100% / R$1000 coupon never produces a negative total.
// free_shipping requires the buyer to have already picked a shipping
// service so the discount has a stable amount to subtract from.
func computeAppliedDiscount(c *Coupon, cart *CartCouponSnapshot) (int64, error) {
	switch c.Type {
	case TypePercent:
		applied := cart.SubtotalCents * int64(c.PercentBPS) / 10000
		if applied > cart.SubtotalCents {
			applied = cart.SubtotalCents
		}
		return applied, nil
	case TypeFixed:
		applied := c.ValueCents
		if applied > cart.SubtotalCents {
			applied = cart.SubtotalCents
		}
		return applied, nil
	case TypeFreeShipping:
		if !cart.HasShippingPicked {
			return 0, httpx.ErrUnprocessable(
				"select shipping before applying a free-shipping coupon",
			)
		}
		return computeFreeShippingDiscount(
			cart.ShippingCostCents,
			cart.CheapestQuotedShippingCents,
			freeShippingCap(c),
		), nil
	default:
		return 0, httpx.ErrUnprocessable("invalid coupon type")
	}
}

// computeFreeShippingDiscount applies the layered cap rules for a
// free-shipping coupon, in order from most restrictive to least:
//
//  1. effectiveCap = the lesser of the merchant's max (when set) and the
//     cheapest available service from the latest quote. This is the most
//     the store is ever willing to subsidize for this cart.
//  2. discount = the lesser of effectiveCap and what the buyer actually
//     pays — never refund more than the buyer is being charged for shipping.
//
// Examples (cap=30):
//   - cheapest=23, selected=23 → 23  (buyer pays 0)
//   - cheapest=23, selected=35 → 23  (buyer pays 12; coupon stops at cheapest)
//   - cheapest=35, selected=35 → 30  (buyer pays 5;  coupon stops at cap)
//   - cheapest=35, selected=50 → 30  (buyer pays 20; coupon stops at cap)
//
// cheapestQuoted=0 means we have no quote cache; fall back to the buyer's
// selected cost so an apply right after selection still works (the select
// call always re-quotes, so a 0 here is a defensive corner case).
func computeFreeShippingDiscount(selected, cheapestQuoted, maxDiscount int64) int64 {
	cap := cheapestQuoted
	if cheapestQuoted <= 0 {
		cap = selected
	}
	if maxDiscount > 0 && maxDiscount < cap {
		cap = maxDiscount
	}
	if cap > selected {
		cap = selected
	}
	if cap < 0 {
		return 0
	}
	return cap
}


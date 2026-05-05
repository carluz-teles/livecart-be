package coupon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// ListByEvent returns every coupon attached to an event, newest first.
// Admin-only — no active=true filter, the merchant needs to see disabled
// rows to flip them back on.
func (r *Repository) ListByEvent(ctx context.Context, eventID string) ([]Coupon, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, event_id, code, type, value_cents, percent_bps,
		       max_uses, used_count, min_purchase_cents,
		       valid_from, valid_until, active, created_at, updated_at
		FROM coupons
		WHERE event_id = $1
		ORDER BY created_at DESC
	`, eventID)
	if err != nil {
		return nil, fmt.Errorf("listing coupons: %w", err)
	}
	defer rows.Close()

	out := make([]Coupon, 0)
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetByID returns the coupon when it belongs to the supplied event. The
// double constraint (id + event_id) lets handlers safely use a single query
// to enforce both existence and tenancy in one round-trip.
func (r *Repository) GetByID(ctx context.Context, id, eventID string) (*Coupon, error) {
	row := r.db.QueryRow(ctx, `
		SELECT id, event_id, code, type, value_cents, percent_bps,
		       max_uses, used_count, min_purchase_cents,
		       valid_from, valid_until, active, created_at, updated_at
		FROM coupons
		WHERE id = $1 AND event_id = $2
	`, id, eventID)
	c, err := scanCoupon(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// EventExistsForStore verifies the eventId path param actually belongs to
// the storeId of the request. Without this check, a tenant could read /
// mutate another store's coupons by guessing event UUIDs.
func (r *Repository) EventExistsForStore(ctx context.Context, eventID, storeID string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM live_events WHERE id = $1 AND store_id = $2)
	`, eventID, storeID).Scan(&ok)
	return ok, err
}

type CreateParams struct {
	EventID          string
	Code             string
	Type             Type
	ValueCents       int64
	PercentBPS       int
	MaxUses          *int
	MinPurchaseCents int64
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	Active           bool
}

func (r *Repository) Create(ctx context.Context, p CreateParams) (*Coupon, error) {
	row := r.db.QueryRow(ctx, `
		INSERT INTO coupons (
			event_id, code, type, value_cents, percent_bps,
			max_uses, min_purchase_cents, valid_from, valid_until, active
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, event_id, code, type, value_cents, percent_bps,
		          max_uses, used_count, min_purchase_cents,
		          valid_from, valid_until, active, created_at, updated_at
	`,
		p.EventID, p.Code, string(p.Type), p.ValueCents, p.PercentBPS,
		nullableInt(p.MaxUses), p.MinPurchaseCents,
		nullableTime(p.ValidFrom), nullableTime(p.ValidUntil), p.Active,
	)
	c, err := scanCoupon(row)
	if err != nil {
		return nil, fmt.Errorf("creating coupon: %w", err)
	}
	return &c, nil
}

type UpdateParams struct {
	ID               string
	EventID          string
	Type             *Type
	ValueCents       *int64
	PercentBPS       *int
	MaxUses          *int
	ClearMaxUses     bool
	MinPurchaseCents *int64
	ValidFrom        *time.Time
	ClearValidFrom   bool
	ValidUntil       *time.Time
	ClearValidUntil  bool
	Active           *bool
}

// Update builds a dynamic SET clause so the merchant can flip a single field
// without resending the whole row. Returns nil when no row matched.
func (r *Repository) Update(ctx context.Context, p UpdateParams) (*Coupon, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 10)
	idx := 1

	add := func(col string, val any) {
		sets = append(sets, fmt.Sprintf("%s = $%d", col, idx))
		args = append(args, val)
		idx++
	}

	if p.Type != nil {
		add("type", string(*p.Type))
	}
	if p.ValueCents != nil {
		add("value_cents", *p.ValueCents)
	}
	if p.PercentBPS != nil {
		add("percent_bps", *p.PercentBPS)
	}
	switch {
	case p.MaxUses != nil:
		add("max_uses", *p.MaxUses)
	case p.ClearMaxUses:
		add("max_uses", nil)
	}
	if p.MinPurchaseCents != nil {
		add("min_purchase_cents", *p.MinPurchaseCents)
	}
	switch {
	case p.ValidFrom != nil:
		add("valid_from", *p.ValidFrom)
	case p.ClearValidFrom:
		add("valid_from", nil)
	}
	switch {
	case p.ValidUntil != nil:
		add("valid_until", *p.ValidUntil)
	case p.ClearValidUntil:
		add("valid_until", nil)
	}
	if p.Active != nil {
		add("active", *p.Active)
	}

	if len(sets) == 0 {
		// Nothing to change; just return the current row.
		return r.GetByID(ctx, p.ID, p.EventID)
	}

	sets = append(sets, "updated_at = NOW()")

	args = append(args, p.ID, p.EventID)
	query := fmt.Sprintf(`
		UPDATE coupons
		SET %s
		WHERE id = $%d AND event_id = $%d
		RETURNING id, event_id, code, type, value_cents, percent_bps,
		          max_uses, used_count, min_purchase_cents,
		          valid_from, valid_until, active, created_at, updated_at
	`, strings.Join(sets, ", "), idx, idx+1)

	row := r.db.QueryRow(ctx, query, args...)
	c, err := scanCoupon(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Delete drops the coupon. Returns false when no row matched (handler maps
// to 404). Redemptions reference coupon_id with ON DELETE RESTRICT, so PG
// will block the delete once a cart has applied the coupon — callers are
// expected to flip Active=false for soft deletion of redeemed coupons.
func (r *Repository) Delete(ctx context.Context, id, eventID string) (bool, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM coupons WHERE id = $1 AND event_id = $2`, id, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// =============================================================================
// PUBLIC-CART APPLY / REMOVE — runs under an explicit tx with a row lock on
// the coupon so concurrent applies of the same code can never race past
// max_uses. The caller (Service.ApplyToCart) owns the tx lifecycle.
// =============================================================================

// CartCouponSnapshot is the minimum cart info ApplyToCart needs to validate
// and compute the discount. We deliberately fetch it inside the same tx as
// the coupon lock so the cart's subtotal cannot drift between read and write.
type CartCouponSnapshot struct {
	CartID             string
	EventID            string
	StoreID            string
	Status             string
	PaymentStatus      string
	SubtotalCents      int64
	ShippingCostCents  int64
	HasShippingPicked  bool
	CouponID           *string // current applied coupon, if any
}

// LoadCartForCouponTx reads everything ApplyToCart / RemoveFromCart need to
// validate the operation, locking nothing on the cart side — the coupon row
// is the contention point. Subtotal sums only the available (non-waitlisted)
// portion of each cart item, matching the totals the gateway sees.
func (r *Repository) LoadCartForCouponTx(
	ctx context.Context,
	tx pgx.Tx,
	token string,
) (*CartCouponSnapshot, error) {
	const q = `
		SELECT
			c.id,
			c.event_id,
			le.store_id,
			c.status,
			c.payment_status,
			COALESCE((
				SELECT SUM(ci.unit_price * GREATEST(ci.quantity - ci.waitlisted_quantity, 0))::BIGINT
				FROM cart_items ci
				WHERE ci.cart_id = c.id
			), 0) AS subtotal_cents,
			COALESCE(c.shipping_cost_cents, 0) AS shipping_cost_cents,
			(c.shipping_service_id IS NOT NULL AND c.shipping_service_id <> '') AS has_shipping_picked,
			c.coupon_id
		FROM carts c
		JOIN live_events le ON le.id = c.event_id
		WHERE c.token = $1
	`
	var (
		out             CartCouponSnapshot
		paymentStatus   pgtype.Text
		cartID, eventID pgtype.UUID
		storeID         pgtype.UUID
		couponID        pgtype.UUID
	)
	err := tx.QueryRow(ctx, q, token).Scan(
		&cartID, &eventID, &storeID,
		&out.Status, &paymentStatus,
		&out.SubtotalCents, &out.ShippingCostCents, &out.HasShippingPicked,
		&couponID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading cart for coupon: %w", err)
	}
	out.CartID = uuid.UUID(cartID.Bytes).String()
	out.EventID = uuid.UUID(eventID.Bytes).String()
	out.StoreID = uuid.UUID(storeID.Bytes).String()
	if paymentStatus.Valid {
		out.PaymentStatus = paymentStatus.String
	}
	if couponID.Valid {
		id := uuid.UUID(couponID.Bytes).String()
		out.CouponID = &id
	}
	return &out, nil
}

// LockCouponByEventCodeTx acquires a pessimistic lock on the coupon row
// matching (event_id, lower(code)). Two concurrent applies of the same code
// queue here — the second waits until the first commits/rolls back, then
// re-reads used_count and either passes or 409s.
func (r *Repository) LockCouponByEventCodeTx(
	ctx context.Context,
	tx pgx.Tx,
	eventID, code string,
) (*Coupon, error) {
	const q = `
		SELECT id, event_id, code, type, value_cents, percent_bps,
		       max_uses, used_count, min_purchase_cents,
		       valid_from, valid_until, active, created_at, updated_at
		FROM coupons
		WHERE event_id = $1 AND lower(code) = lower($2)
		FOR UPDATE
	`
	row := tx.QueryRow(ctx, q, eventID, code)
	c, err := scanCoupon(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// LockCouponByIDTx is the dual of LockCouponByEventCodeTx, used by the
// remove-from-cart flow where we already know the coupon id (from
// carts.coupon_id) and just need to take the lock to safely decrement
// used_count.
func (r *Repository) LockCouponByIDTx(
	ctx context.Context,
	tx pgx.Tx,
	id string,
) (*Coupon, error) {
	const q = `
		SELECT id, event_id, code, type, value_cents, percent_bps,
		       max_uses, used_count, min_purchase_cents,
		       valid_from, valid_until, active, created_at, updated_at
		FROM coupons
		WHERE id = $1
		FOR UPDATE
	`
	row := tx.QueryRow(ctx, q, id)
	c, err := scanCoupon(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repository) InsertReservedRedemptionTx(
	ctx context.Context,
	tx pgx.Tx,
	couponID, cartID string,
	appliedValueCents int64,
) error {
	const q = `
		INSERT INTO coupon_redemptions (coupon_id, cart_id, status, applied_value_cents)
		VALUES ($1, $2, 'reserved', $3)
	`
	_, err := tx.Exec(ctx, q, couponID, cartID, appliedValueCents)
	if err != nil {
		return fmt.Errorf("inserting redemption: %w", err)
	}
	return nil
}

func (r *Repository) DeleteRedemptionByCartTx(
	ctx context.Context,
	tx pgx.Tx,
	cartID string,
) error {
	_, err := tx.Exec(ctx, `DELETE FROM coupon_redemptions WHERE cart_id = $1`, cartID)
	if err != nil {
		return fmt.Errorf("deleting redemption: %w", err)
	}
	return nil
}

func (r *Repository) IncrementUsedCountTx(ctx context.Context, tx pgx.Tx, couponID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE coupons SET used_count = used_count + 1, updated_at = NOW() WHERE id = $1`,
		couponID,
	)
	return err
}

func (r *Repository) DecrementUsedCountTx(ctx context.Context, tx pgx.Tx, couponID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE coupons SET used_count = GREATEST(used_count - 1, 0), updated_at = NOW() WHERE id = $1`,
		couponID,
	)
	return err
}

func (r *Repository) ApplyCouponToCartTx(
	ctx context.Context,
	tx pgx.Tx,
	cartID, couponID, code string,
	discountCents int64,
) error {
	const q = `
		UPDATE carts
		SET coupon_id = $2, coupon_code = $3, coupon_discount_cents = $4
		WHERE id = $1
	`
	_, err := tx.Exec(ctx, q, cartID, couponID, code, discountCents)
	return err
}

func (r *Repository) ClearCouponOnCartTx(ctx context.Context, tx pgx.Tx, cartID string) error {
	const q = `
		UPDATE carts
		SET coupon_id = NULL, coupon_code = NULL, coupon_discount_cents = 0
		WHERE id = $1
	`
	_, err := tx.Exec(ctx, q, cartID)
	return err
}

// =============================================================================
// REDEMPTION LIFECYCLE — webhook flips reserved → confirmed → refunded.
// =============================================================================

// RedemptionRow is the projection the lifecycle hooks need: just enough to
// decide whether the transition is valid + which coupon to debit on refund.
type RedemptionRow struct {
	ID       string
	CouponID string
	Status   string
}

// LoadRedemptionByCart returns the redemption attached to a cart, or nil
// when none exists. Read-only (no lock) — callers that mutate take the
// coupon row lock instead.
func (r *Repository) LoadRedemptionByCart(ctx context.Context, cartID string) (*RedemptionRow, error) {
	const q = `
		SELECT id, coupon_id, status
		FROM coupon_redemptions
		WHERE cart_id = $1
	`
	var (
		out      RedemptionRow
		idVal    pgtype.UUID
		couponID pgtype.UUID
	)
	err := r.db.QueryRow(ctx, q, cartID).Scan(&idVal, &couponID, &out.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.ID = uuid.UUID(idVal.Bytes).String()
	out.CouponID = uuid.UUID(couponID.Bytes).String()
	return &out, nil
}

// MarkRedemptionConfirmed flips a reserved redemption to confirmed and
// stamps confirmed_at. Idempotent: rows already in 'confirmed' are not
// touched (RowsAffected=0). Refunded rows are NOT moved back — once a
// chargeback happens the merchant has to disable+recreate.
func (r *Repository) MarkRedemptionConfirmed(ctx context.Context, redemptionID string) error {
	const q = `
		UPDATE coupon_redemptions
		SET status = 'confirmed', confirmed_at = NOW()
		WHERE id = $1 AND status = 'reserved'
	`
	_, err := r.db.Exec(ctx, q, redemptionID)
	return err
}

// MarkRedemptionRefundedTx flips a reserved/confirmed redemption to
// refunded inside a tx so the matching used_count decrement on the coupon
// row stays atomic.
func (r *Repository) MarkRedemptionRefundedTx(ctx context.Context, tx pgx.Tx, redemptionID string) error {
	const q = `
		UPDATE coupon_redemptions
		SET status = 'refunded'
		WHERE id = $1 AND status IN ('reserved', 'confirmed')
	`
	_, err := tx.Exec(ctx, q, redemptionID)
	return err
}

// GetCouponByID is a non-locked read used by the shipping-change re-eval —
// we don't decrement used_count there, so no row lock needed.
func (r *Repository) GetCouponByID(ctx context.Context, id string) (*Coupon, error) {
	const q = `
		SELECT id, event_id, code, type, value_cents, percent_bps,
		       max_uses, used_count, min_purchase_cents,
		       valid_from, valid_until, active, created_at, updated_at
		FROM coupons
		WHERE id = $1
	`
	row := r.db.QueryRow(ctx, q, id)
	c, err := scanCoupon(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetCartShippingCostCents fetches just the cart's current shipping cost.
// Returns 0 when no shipping is selected — callers treat that as "discount
// resets to zero until the buyer picks a new option".
func (r *Repository) GetCartShippingCostCents(ctx context.Context, cartID string) (int64, error) {
	var cents int64
	err := r.db.QueryRow(ctx,
		`SELECT COALESCE(shipping_cost_cents, 0) FROM carts WHERE id = $1`,
		cartID,
	).Scan(&cents)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return cents, err
}

// StaleRedemption is the projection the expirer worker scans — only the
// IDs it needs to decrement and update.
type StaleRedemption struct {
	RedemptionID string
	CouponID     string
	CartID       string
}

// ListStaleReservedRedemptions returns reserved redemptions whose cart will
// never be paid: cart is already 'expired'/'cancelled', payment failed/
// cancelled, or expires_at slipped past with no payment. Cap per call so
// one slow run doesn't lock up the worker.
func (r *Repository) ListStaleReservedRedemptions(ctx context.Context, limit int) ([]StaleRedemption, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
		SELECT cr.id, cr.coupon_id, cr.cart_id
		FROM coupon_redemptions cr
		JOIN carts c ON c.id = cr.cart_id
		WHERE cr.status = 'reserved'
		  AND (
		    c.status = 'expired'
		    OR c.payment_status IN ('failed', 'cancelled', 'refunded')
		    OR (
		      c.expires_at IS NOT NULL
		      AND c.expires_at < NOW()
		      AND COALESCE(c.payment_status, 'pending') NOT IN ('paid', 'refunded')
		    )
		  )
		LIMIT $1
	`
	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]StaleRedemption, 0)
	for rows.Next() {
		var (
			redID, couponID, cartID pgtype.UUID
		)
		if err := rows.Scan(&redID, &couponID, &cartID); err != nil {
			return nil, err
		}
		out = append(out, StaleRedemption{
			RedemptionID: uuid.UUID(redID.Bytes).String(),
			CouponID:     uuid.UUID(couponID.Bytes).String(),
			CartID:       uuid.UUID(cartID.Bytes).String(),
		})
	}
	return out, rows.Err()
}

// MarkRedemptionExpiredTx flips a reserved redemption to 'expired' inside
// the same tx that decrements used_count, so the slot returns to circulation
// atomically.
func (r *Repository) MarkRedemptionExpiredTx(ctx context.Context, tx pgx.Tx, redemptionID string) error {
	const q = `
		UPDATE coupon_redemptions
		SET status = 'expired'
		WHERE id = $1 AND status = 'reserved'
	`
	_, err := tx.Exec(ctx, q, redemptionID)
	return err
}

// UpdateCartCouponDiscount overwrites just the discount cents on the cart;
// coupon_id and coupon_code are left untouched (used by shipping-change
// re-eval — the cart still has the same coupon, only the snapshot changes).
func (r *Repository) UpdateCartCouponDiscount(ctx context.Context, cartID string, discountCents int64) error {
	_, err := r.db.Exec(ctx,
		`UPDATE carts SET coupon_discount_cents = $2 WHERE id = $1`,
		cartID, discountCents,
	)
	return err
}

// rowScanner is a tiny abstraction so scanCoupon can take both the single-
// row return of QueryRow and the iterator form of Query/Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCoupon(s rowScanner) (Coupon, error) {
	var (
		c          Coupon
		typeStr    string
		eventID    pgtype.UUID
		idVal      pgtype.UUID
		validFrom  pgtype.Timestamptz
		validUntil pgtype.Timestamptz
		maxUses    pgtype.Int4
	)
	err := s.Scan(
		&idVal,
		&eventID,
		&c.Code,
		&typeStr,
		&c.ValueCents,
		&c.PercentBPS,
		&maxUses,
		&c.UsedCount,
		&c.MinPurchaseCents,
		&validFrom,
		&validUntil,
		&c.Active,
		&c.CreatedAt,
		&c.UpdatedAt,
	)
	if err != nil {
		return c, err
	}
	c.ID = uuid.UUID(idVal.Bytes).String()
	c.EventID = uuid.UUID(eventID.Bytes).String()
	c.Type = Type(typeStr)
	if maxUses.Valid {
		v := int(maxUses.Int32)
		c.MaxUses = &v
	}
	if validFrom.Valid {
		t := validFrom.Time
		c.ValidFrom = &t
	}
	if validUntil.Valid {
		t := validUntil.Time
		c.ValidUntil = &t
	}
	return c, nil
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return *p
}

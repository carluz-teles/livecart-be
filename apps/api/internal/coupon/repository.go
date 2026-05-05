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

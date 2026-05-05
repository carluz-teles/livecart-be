package coupon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{repo: repo, logger: logger}
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


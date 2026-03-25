package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

func (r *Repository) Create(ctx context.Context, params CreateStoreParams) (StoreRow, error) {
	row, err := r.q.CreateStore(ctx, sqlc.CreateStoreParams{
		Name: params.Name,
		Slug: params.Slug,
	})
	if err != nil {
		return StoreRow{}, fmt.Errorf("inserting store: %w", err)
	}

	return toStoreRow(row), nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*StoreRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetStoreByID(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("getting store: %w", err)
	}

	out := toStoreRow(row)
	return &out, nil
}

func (r *Repository) GetBySlug(ctx context.Context, slug string) (*StoreRow, error) {
	row, err := r.q.GetStoreBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("getting store by slug: %w", err)
	}

	out := toStoreRow(row)
	return &out, nil
}

func (r *Repository) Update(ctx context.Context, params UpdateStoreParams) (StoreRow, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return StoreRow{}, err
	}

	row, err := r.q.UpdateStore(ctx, sqlc.UpdateStoreParams{
		ID:             uid,
		Name:           params.Name,
		WhatsappNumber: pgtype.Text{String: params.WhatsappNumber, Valid: params.WhatsappNumber != ""},
		EmailAddress:   pgtype.Text{String: params.EmailAddress, Valid: params.EmailAddress != ""},
		SmsNumber:      pgtype.Text{String: params.SMSNumber, Valid: params.SMSNumber != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoreRow{}, httpx.ErrNotFound("store not found")
		}
		return StoreRow{}, fmt.Errorf("updating store: %w", err)
	}

	return toStoreRow(row), nil
}

func (r *Repository) UpdateCartSettings(ctx context.Context, params UpdateCartSettingsParams) (StoreRow, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return StoreRow{}, err
	}

	row, err := r.q.UpdateStoreCartSettings(ctx, sqlc.UpdateStoreCartSettingsParams{
		ID:                        uid,
		CartEnabled:               params.Enabled,
		CartExpirationMinutes:     int32(params.ExpirationMinutes),
		CartReserveStock:          params.ReserveStock,
		CartMaxItems:              int32(params.MaxItems),
		CartMaxQuantityPerItem:    int32(params.MaxQuantityPerItem),
		CartNotifyBeforeExpiration: params.NotifyBeforeExpiration,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoreRow{}, httpx.ErrNotFound("store not found")
		}
		return StoreRow{}, fmt.Errorf("updating cart settings: %w", err)
	}

	return toStoreRow(row), nil
}

func toStoreRow(row sqlc.Store) StoreRow {
	var whatsapp, email, sms *string
	if row.WhatsappNumber.Valid {
		whatsapp = &row.WhatsappNumber.String
	}
	if row.EmailAddress.Valid {
		email = &row.EmailAddress.String
	}
	if row.SmsNumber.Valid {
		sms = &row.SmsNumber.String
	}

	return StoreRow{
		ID:             row.ID.String(),
		Name:           row.Name,
		Slug:           row.Slug,
		Active:         row.Active.Bool,
		WhatsappNumber: whatsapp,
		EmailAddress:   email,
		SMSNumber:      sms,
		CartSettings: CartSettingsDTO{
			Enabled:                row.CartEnabled,
			ExpirationMinutes:      int(row.CartExpirationMinutes),
			ReserveStock:           row.CartReserveStock,
			MaxItems:               int(row.CartMaxItems),
			MaxQuantityPerItem:     int(row.CartMaxQuantityPerItem),
			NotifyBeforeExpiration: row.CartNotifyBeforeExpiration,
		},
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}

func (r *Repository) CompleteOnboarding(ctx context.Context, storeID string) error {
	uid, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	err = r.q.CompleteOnboarding(ctx, uid)
	if err != nil {
		return fmt.Errorf("completing onboarding: %w", err)
	}

	return nil
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}

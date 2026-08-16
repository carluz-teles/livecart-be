package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	storedomain "livecart/apps/api/internal/store/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

func (r *Repository) Create(ctx context.Context, params CreateStoreParams) (*storedomain.Store, error) {
	row, err := r.q.CreateStore(ctx, sqlc.CreateStoreParams{
		Name: params.Name,
		Slug: params.Slug,
	})
	if err != nil {
		return nil, fmt.Errorf("inserting store: %w", err)
	}

	return rowToEntity(row), nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*storedomain.Store, error) {
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

	return rowToEntity(row), nil
}

// GetSlugByID resolves only the slug — lookup barato usado para enriquecer
// logs em webhooks/workers que só conhecem o store_id (httpx.StoreSlugResolver).
func (r *Repository) GetSlugByID(ctx context.Context, id string) (string, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return "", err
	}
	slug, err := r.q.GetStoreSlugByID(ctx, uid)
	if err != nil {
		return "", fmt.Errorf("getting store slug: %w", err)
	}
	return slug, nil
}

func (r *Repository) GetBySlug(ctx context.Context, slug string) (*storedomain.Store, error) {
	row, err := r.q.GetStoreBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("getting store by slug: %w", err)
	}

	return rowToEntity(row), nil
}

func (r *Repository) Update(ctx context.Context, params UpdateStoreParams) (*storedomain.Store, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.UpdateStore(ctx, sqlc.UpdateStoreParams{
		ID:                   uid,
		Name:                 params.Name,
		WhatsappNumber:       pgtype.Text{String: params.WhatsappNumber, Valid: params.WhatsappNumber != ""},
		EmailAddress:         pgtype.Text{String: params.EmailAddress, Valid: params.EmailAddress != ""},
		SmsNumber:            pgtype.Text{String: params.SMSNumber, Valid: params.SMSNumber != ""},
		Description:          pgtype.Text{String: params.Description, Valid: params.Description != ""},
		Website:              pgtype.Text{String: params.Website, Valid: params.Website != ""},
		LogoUrl:              pgtype.Text{String: params.LogoURL, Valid: params.LogoURL != ""},
		AddressStreet:        pgtype.Text{String: params.AddressStreet, Valid: params.AddressStreet != ""},
		AddressCity:          pgtype.Text{String: params.AddressCity, Valid: params.AddressCity != ""},
		AddressState:         pgtype.Text{String: params.AddressState, Valid: params.AddressState != ""},
		AddressZip:           pgtype.Text{String: params.AddressZip, Valid: params.AddressZip != ""},
		AddressCountry:       pgtype.Text{String: params.AddressCountry, Valid: params.AddressCountry != ""},
		Cnpj:                 pgtype.Text{String: params.CNPJ, Valid: params.CNPJ != ""},
		AddressNumber:        pgtype.Text{String: params.AddressNumber, Valid: params.AddressNumber != ""},
		AddressComplement:    pgtype.Text{String: params.AddressComplement, Valid: params.AddressComplement != ""},
		AddressDistrict:      pgtype.Text{String: params.AddressDistrict, Valid: params.AddressDistrict != ""},
		AddressStateRegister: pgtype.Text{String: params.AddressStateRegister, Valid: params.AddressStateRegister != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("updating store: %w", err)
	}

	return rowToEntity(row), nil
}

func (r *Repository) UpdateShippingDefaults(ctx context.Context, params UpdateShippingDefaultsParams) (*storedomain.Store, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.UpdateStoreShippingDefaults(ctx, sqlc.UpdateStoreShippingDefaultsParams{
		ID:                        uid,
		DefaultPackageWeightGrams: int32(params.PackageWeightGrams),
		DefaultPackageFormat:      params.PackageFormat,
		DefaultHeightCm:           intPtrToPgInt4(params.HeightCm),
		DefaultWidthCm:            intPtrToPgInt4(params.WidthCm),
		DefaultLengthCm:           intPtrToPgInt4(params.LengthCm),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("updating shipping defaults: %w", err)
	}

	return rowToEntity(row), nil
}

func (r *Repository) UpdateCartSettings(ctx context.Context, params UpdateCartSettingsParams) (*storedomain.Store, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return nil, err
	}

	// Convert checkout methods to JSON
	checkoutMethodsJSON, err := json.Marshal(params.CheckoutSendMethods)
	if err != nil {
		return nil, fmt.Errorf("marshaling checkout methods: %w", err)
	}

	row, err := r.q.UpdateStoreCartSettings(ctx, sqlc.UpdateStoreCartSettingsParams{
		ID:                            uid,
		CartEnabled:                   params.Enabled,
		CartExpirationMinutes:         int32(params.ExpirationMinutes),
		CartReserveStock:              params.ReserveStock,
		AllowStorePickup:              params.AllowStorePickup,
		MinInstallmentCents:           int32(params.MinInstallmentCents),
		CartMaxQuantityPerItem:        int32(params.MaxQuantityPerItem),
		CartAllowEdit:                 params.AllowEdit,
		CartRealTime:                  params.RealTimeCart,
		SendOnLiveEnd:                 params.SendOnLiveEnd,
		CheckoutSendMethods:           checkoutMethodsJSON,
		CartMessageCooldownSeconds:    int32(params.MessageCooldownSeconds),
		CartSendExpirationReminder:    params.SendExpirationReminder,
		CartExpirationReminderMinutes: int32(params.ExpirationReminderMinutes),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("updating cart settings: %w", err)
	}

	return rowToEntity(row), nil
}

func (r *Repository) GetByUserID(ctx context.Context, userID string) (*storedomain.Store, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetStoreByUserID(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("getting store by user id: %w", err)
	}

	return rowToEntity(row), nil
}

func (r *Repository) UpdateLogoURL(ctx context.Context, storeID string, logoURL string) (*storedomain.Store, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.UpdateStoreLogoURL(ctx, sqlc.UpdateStoreLogoURLParams{
		ID:      uid,
		LogoUrl: pgtype.Text{String: logoURL, Valid: logoURL != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("store not found")
		}
		return nil, fmt.Errorf("updating logo url: %w", err)
	}

	return rowToEntity(row), nil
}

// rowToEntity maps a persisted sqlc.Store row into the domain entity
// (Reconstruct — no validation, the row is trusted).
func rowToEntity(row sqlc.Store) *storedomain.Store {
	whatsapp := textPtr(row.WhatsappNumber)
	email := textPtr(row.EmailAddress)
	sms := textPtr(row.SmsNumber)
	description := textPtr(row.Description)
	website := textPtr(row.Website)
	logoURL := textPtr(row.LogoUrl)
	cnpj := textPtr(row.Cnpj)

	address := storedomain.ReconstructAddress(
		textStr(row.AddressStreet),
		textStr(row.AddressNumber),
		textStr(row.AddressComplement),
		textStr(row.AddressDistrict),
		textStr(row.AddressCity),
		textStr(row.AddressState),
		textStr(row.AddressZip),
		textStr(row.AddressCountry),
		textStr(row.AddressStateRegister),
	)

	// Parse checkout send methods from JSON (fallback to the platform default).
	var checkoutMethods []string
	if len(row.CheckoutSendMethods) > 0 {
		_ = json.Unmarshal(row.CheckoutSendMethods, &checkoutMethods)
	}
	if checkoutMethods == nil {
		checkoutMethods = []string{"public_link", "manual"}
	}

	cartSettings := storedomain.ReconstructCartSettings(
		row.CartEnabled,
		int(row.CartExpirationMinutes),
		row.CartReserveStock,
		row.AllowStorePickup,
		int(row.CartMaxQuantityPerItem),
		row.CartAllowEdit,
		checkoutMethods,
		row.CartRealTime,
		row.SendOnLiveEnd,
		int(row.CartMessageCooldownSeconds),
		row.CartSendExpirationReminder,
		int(row.CartExpirationReminderMinutes),
	)

	shippingDefaults := storedomain.ReconstructShippingDefaults(
		int(row.DefaultPackageWeightGrams),
		defaultPackageFormat(row.DefaultPackageFormat),
		pgInt4ToIntPtr(row.DefaultHeightCm),
		pgInt4ToIntPtr(row.DefaultWidthCm),
		pgInt4ToIntPtr(row.DefaultLengthCm),
	)

	return storedomain.Reconstruct(
		vo.MustNewStoreID(row.ID.String()),
		row.Name,
		storedomain.MustSlug(row.Slug),
		row.Active.Bool,
		whatsapp,
		email,
		sms,
		description,
		website,
		logoURL,
		cnpj,
		address,
		cartSettings,
		shippingDefaults,
		row.CreatedAt.Time,
		row.UpdatedAt.Time,
	)
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func textStr(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func defaultPackageFormat(s string) string {
	if s == "" {
		return "box"
	}
	return s
}

func intPtrToPgInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func pgInt4ToIntPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}

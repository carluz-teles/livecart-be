package store

import (
	"context"
	"encoding/json"
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
			return StoreRow{}, httpx.ErrNotFound("store not found")
		}
		return StoreRow{}, fmt.Errorf("updating store: %w", err)
	}

	return toStoreRow(row), nil
}

func (r *Repository) UpdateShippingDefaults(ctx context.Context, params UpdateShippingDefaultsParams) (StoreRow, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return StoreRow{}, err
	}

	row, err := r.q.UpdateStoreShippingDefaults(ctx, sqlc.UpdateStoreShippingDefaultsParams{
		ID:                        uid,
		DefaultPackageWeightGrams: int32(params.PackageWeightGrams),
		DefaultPackageFormat:      params.PackageFormat,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoreRow{}, httpx.ErrNotFound("store not found")
		}
		return StoreRow{}, fmt.Errorf("updating shipping defaults: %w", err)
	}

	return toStoreRow(row), nil
}

func (r *Repository) UpdateCartSettings(ctx context.Context, params UpdateCartSettingsParams) (StoreRow, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return StoreRow{}, err
	}

	// Convert checkout methods to JSON
	checkoutMethodsJSON, err := json.Marshal(params.CheckoutSendMethods)
	if err != nil {
		return StoreRow{}, fmt.Errorf("marshaling checkout methods: %w", err)
	}

	row, err := r.q.UpdateStoreCartSettings(ctx, sqlc.UpdateStoreCartSettingsParams{
		ID:                            uid,
		CartEnabled:                   params.Enabled,
		CartExpirationMinutes:         int32(params.ExpirationMinutes),
		CartReserveStock:              params.ReserveStock,
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
			return StoreRow{}, httpx.ErrNotFound("store not found")
		}
		return StoreRow{}, fmt.Errorf("updating cart settings: %w", err)
	}

	return toStoreRow(row), nil
}

func (r *Repository) GetByUserID(ctx context.Context, userID string) (*StoreRow, error) {
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

	out := toStoreRow(row)
	return &out, nil
}

func (r *Repository) UpdateLogoURL(ctx context.Context, storeID string, logoURL string) (StoreRow, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return StoreRow{}, err
	}

	row, err := r.q.UpdateStoreLogoURL(ctx, sqlc.UpdateStoreLogoURLParams{
		ID:      uid,
		LogoUrl: pgtype.Text{String: logoURL, Valid: logoURL != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoreRow{}, httpx.ErrNotFound("store not found")
		}
		return StoreRow{}, fmt.Errorf("updating logo url: %w", err)
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

	var description, website, logoURL *string
	if row.Description.Valid {
		description = &row.Description.String
	}
	if row.Website.Valid {
		website = &row.Website.String
	}
	if row.LogoUrl.Valid {
		logoURL = &row.LogoUrl.String
	}

	var addressStreet, addressCity, addressState, addressZip, addressCountry *string
	if row.AddressStreet.Valid {
		addressStreet = &row.AddressStreet.String
	}
	if row.AddressCity.Valid {
		addressCity = &row.AddressCity.String
	}
	if row.AddressState.Valid {
		addressState = &row.AddressState.String
	}
	if row.AddressZip.Valid {
		addressZip = &row.AddressZip.String
	}
	if row.AddressCountry.Valid {
		addressCountry = &row.AddressCountry.String
	}

	var addressNumber, addressComplement, addressDistrict, addressStateRegister *string
	if row.AddressNumber.Valid {
		addressNumber = &row.AddressNumber.String
	}
	if row.AddressComplement.Valid {
		addressComplement = &row.AddressComplement.String
	}
	if row.AddressDistrict.Valid {
		addressDistrict = &row.AddressDistrict.String
	}
	if row.AddressStateRegister.Valid {
		addressStateRegister = &row.AddressStateRegister.String
	}

	var cnpj *string
	if row.Cnpj.Valid {
		cnpj = &row.Cnpj.String
	}

	// Parse checkout send methods from JSON
	var checkoutMethods []string
	if len(row.CheckoutSendMethods) > 0 {
		_ = json.Unmarshal(row.CheckoutSendMethods, &checkoutMethods)
	}
	if checkoutMethods == nil {
		checkoutMethods = []string{"public_link", "manual"}
	}

	return StoreRow{
		ID:                   row.ID.String(),
		Name:                 row.Name,
		Slug:                 row.Slug,
		Active:               row.Active.Bool,
		WhatsappNumber:       whatsapp,
		EmailAddress:         email,
		SMSNumber:            sms,
		Description:          description,
		Website:              website,
		LogoURL:              logoURL,
		AddressStreet:        addressStreet,
		AddressNumber:        addressNumber,
		AddressComplement:    addressComplement,
		AddressDistrict:      addressDistrict,
		AddressCity:          addressCity,
		AddressState:         addressState,
		AddressZip:           addressZip,
		AddressCountry:       addressCountry,
		AddressStateRegister: addressStateRegister,
		CNPJ:                 cnpj,
		CartSettings: CartSettingsDTO{
			Enabled:                   row.CartEnabled,
			ExpirationMinutes:         int(row.CartExpirationMinutes),
			ReserveStock:              row.CartReserveStock,
			MaxQuantityPerItem:        int(row.CartMaxQuantityPerItem),
			AllowEdit:                 row.CartAllowEdit,
			CheckoutSendMethods:       checkoutMethods,
			RealTimeCart:              row.CartRealTime,
			SendOnLiveEnd:             row.SendOnLiveEnd,
			MessageCooldownSeconds:    int(row.CartMessageCooldownSeconds),
			SendExpirationReminder:    row.CartSendExpirationReminder,
			ExpirationReminderMinutes: int(row.CartExpirationReminderMinutes),
		},
		ShippingDefaults: ShippingDefaultsDTO{
			PackageWeightGrams: int(row.DefaultPackageWeightGrams),
			PackageFormat:      defaultPackageFormat(row.DefaultPackageFormat),
		},
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}

func defaultPackageFormat(s string) string {
	if s == "" {
		return "box"
	}
	return s
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}

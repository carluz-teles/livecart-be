package payment

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

type Repository struct {
	queries *sqlc.Queries
}

func NewRepository(queries *sqlc.Queries) *Repository {
	return &Repository{queries: queries}
}

func (r *Repository) Create(ctx context.Context, input CreatePaymentInput) (*Payment, error) {
	params := sqlc.CreatePaymentParams{
		CartID:      uuidToPgtype(input.CartID),
		Provider:    input.Provider,
		AmountCents: input.AmountCents,
		Status:      string(input.Status),
	}

	if input.IntegrationID != nil {
		params.IntegrationID = uuidToPgtype(*input.IntegrationID)
	}
	if input.ExternalPaymentID != nil {
		params.ExternalPaymentID = pgtype.Text{String: *input.ExternalPaymentID, Valid: true}
	}
	if input.Currency != "" {
		params.Currency = pgtype.Text{String: input.Currency, Valid: true}
	} else {
		params.Currency = pgtype.Text{String: "BRL", Valid: true}
	}
	if input.Method != nil {
		params.Method = pgtype.Text{String: *input.Method, Valid: true}
	}
	if input.StatusDetail != nil {
		params.StatusDetail = pgtype.Text{String: *input.StatusDetail, Valid: true}
	}
	if input.ProviderResponse != nil {
		params.ProviderResponse = input.ProviderResponse
	}
	if input.IdempotencyKey != nil {
		params.IdempotencyKey = pgtype.Text{String: *input.IdempotencyKey, Valid: true}
	}

	row, err := r.queries.CreatePayment(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("creating payment: %w", err)
	}

	return r.rowToPayment(row), nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*Payment, error) {
	row, err := r.queries.GetPaymentByID(ctx, uuidToPgtype(id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting payment by id: %w", err)
	}

	return r.rowToPayment(row), nil
}

func (r *Repository) GetByExternalID(ctx context.Context, externalID string) (*Payment, error) {
	row, err := r.queries.GetPaymentByExternalID(ctx, pgtype.Text{String: externalID, Valid: true})
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting payment by external id: %w", err)
	}

	return r.rowToPayment(row), nil
}

func (r *Repository) GetByIdempotencyKey(ctx context.Context, key string) (*Payment, error) {
	row, err := r.queries.GetPaymentByIdempotencyKey(ctx, pgtype.Text{String: key, Valid: true})
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting payment by idempotency key: %w", err)
	}

	return r.rowToPayment(row), nil
}

func (r *Repository) ListByCart(ctx context.Context, cartID uuid.UUID) ([]*Payment, error) {
	rows, err := r.queries.ListPaymentsByCart(ctx, uuidToPgtype(cartID))
	if err != nil {
		return nil, fmt.Errorf("listing payments by cart: %w", err)
	}

	payments := make([]*Payment, len(rows))
	for i, row := range rows {
		payments[i] = r.rowToPayment(row)
	}

	return payments, nil
}

func (r *Repository) GetLatestByCart(ctx context.Context, cartID uuid.UUID) (*Payment, error) {
	row, err := r.queries.GetLatestPaymentByCart(ctx, uuidToPgtype(cartID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting latest payment by cart: %w", err)
	}

	return r.rowToPayment(row), nil
}

func (r *Repository) UpdateStatus(ctx context.Context, id uuid.UUID, input UpdatePaymentStatusInput) error {
	params := sqlc.UpdatePaymentStatusParams{
		ID:     uuidToPgtype(id),
		Status: string(input.Status),
	}
	if input.StatusDetail != nil {
		params.StatusDetail = pgtype.Text{String: *input.StatusDetail, Valid: true}
	}
	if input.PaidAt != nil {
		params.PaidAt = pgtype.Timestamptz{Time: *input.PaidAt, Valid: true}
	}

	err := r.queries.UpdatePaymentStatus(ctx, params)
	if err != nil {
		return fmt.Errorf("updating payment status: %w", err)
	}

	return nil
}

func (r *Repository) UpdateByExternalID(ctx context.Context, input UpdatePaymentByExternalIDInput) error {
	params := sqlc.UpdatePaymentByExternalIDParams{
		ExternalPaymentID: pgtype.Text{String: input.ExternalPaymentID, Valid: true},
		Status:            string(input.Status),
	}
	if input.StatusDetail != nil {
		params.StatusDetail = pgtype.Text{String: *input.StatusDetail, Valid: true}
	}
	if input.PaidAt != nil {
		params.PaidAt = pgtype.Timestamptz{Time: *input.PaidAt, Valid: true}
	}
	if input.Method != nil {
		params.Method = pgtype.Text{String: *input.Method, Valid: true}
	}
	if input.ProviderResponse != nil {
		params.ProviderResponse = input.ProviderResponse
	}

	err := r.queries.UpdatePaymentByExternalID(ctx, params)
	if err != nil {
		return fmt.Errorf("updating payment by external id: %w", err)
	}

	return nil
}

func (r *Repository) ListByStore(ctx context.Context, storeID uuid.UUID, limit, offset int32) ([]*Payment, error) {
	rows, err := r.queries.ListPaymentsByStore(ctx, sqlc.ListPaymentsByStoreParams{
		StoreID: uuidToPgtype(storeID),
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		return nil, fmt.Errorf("listing payments by store: %w", err)
	}

	payments := make([]*Payment, len(rows))
	for i, row := range rows {
		payments[i] = r.rowToPayment(row)
	}

	return payments, nil
}

func (r *Repository) CountByStatus(ctx context.Context, storeID uuid.UUID) ([]PaymentStatusCount, error) {
	rows, err := r.queries.CountPaymentsByStatus(ctx, uuidToPgtype(storeID))
	if err != nil {
		return nil, fmt.Errorf("counting payments by status: %w", err)
	}

	counts := make([]PaymentStatusCount, len(rows))
	for i, row := range rows {
		counts[i] = PaymentStatusCount{
			Status: row.Status,
			Count:  int(row.Count),
		}
	}

	return counts, nil
}

func (r *Repository) GetStats(ctx context.Context, storeID uuid.UUID) (*PaymentStats, error) {
	row, err := r.queries.GetPaymentStats(ctx, uuidToPgtype(storeID))
	if err != nil {
		return nil, fmt.Errorf("getting payment stats: %w", err)
	}

	return &PaymentStats{
		TotalPayments:       int(row.TotalPayments),
		ApprovedPayments:    int(row.ApprovedPayments),
		TotalApprovedAmount: row.TotalApprovedAmount,
	}, nil
}

// Helper functions
func uuidToPgtype(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgtypeToUUID(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil
	}
	return id.Bytes
}

func pgtypeToUUIDPtr(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	result := uuid.UUID(id.Bytes)
	return &result
}

func (r *Repository) rowToPayment(row sqlc.Payment) *Payment {
	p := &Payment{
		ID:          pgtypeToUUID(row.ID),
		CartID:      pgtypeToUUID(row.CartID),
		Provider:    row.Provider,
		AmountCents: row.AmountCents,
		Currency:    "BRL",
		Status:      PaymentStatus(row.Status),
	}

	if row.IntegrationID.Valid {
		id := uuid.UUID(row.IntegrationID.Bytes)
		p.IntegrationID = &id
	}
	if row.ExternalPaymentID.Valid {
		p.ExternalPaymentID = &row.ExternalPaymentID.String
	}
	if row.Currency.Valid {
		p.Currency = row.Currency.String
	}
	if row.Method.Valid {
		p.Method = &row.Method.String
	}
	if row.StatusDetail.Valid {
		p.StatusDetail = &row.StatusDetail.String
	}
	if row.ProviderResponse != nil {
		p.ProviderResponse = json.RawMessage(row.ProviderResponse)
	}
	if row.CreatedAt.Valid {
		p.CreatedAt = row.CreatedAt.Time
	}
	if row.UpdatedAt.Valid {
		p.UpdatedAt = row.UpdatedAt.Time
	}
	if row.PaidAt.Valid {
		p.PaidAt = &row.PaidAt.Time
	}
	if row.IdempotencyKey.Valid {
		p.IdempotencyKey = &row.IdempotencyKey.String
	}

	return p
}

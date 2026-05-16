package customer

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

// Upsert creates or updates a customer (by store_id + platform_user_id)
func (r *Repository) Upsert(ctx context.Context, input UpsertCustomerInput) (*CustomerRow, error) {
	params := sqlc.UpsertCustomerParams{
		StoreID:        uuidToPgtype(input.StoreID),
		PlatformUserID: input.PlatformUserID,
		PlatformHandle: input.PlatformHandle,
	}
	if input.Email != nil {
		params.Email = pgtype.Text{String: *input.Email, Valid: true}
	}
	if input.Phone != nil {
		params.Phone = pgtype.Text{String: *input.Phone, Valid: true}
	}

	row, err := r.queries.UpsertCustomer(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("upserting customer: %w", err)
	}

	return r.sqlcToCustomerRow(row), nil
}

// GetByID returns a customer by its UUID
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*CustomerRow, error) {
	row, err := r.queries.GetCustomerByID(ctx, uuidToPgtype(id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting customer by id: %w", err)
	}

	return r.sqlcToCustomerRow(row), nil
}

// GetByPlatformUser returns a customer by store_id + platform_user_id
func (r *Repository) GetByPlatformUser(ctx context.Context, storeID uuid.UUID, platformUserID string) (*CustomerRow, error) {
	row, err := r.queries.GetCustomerByPlatformUser(ctx, sqlc.GetCustomerByPlatformUserParams{
		StoreID:        uuidToPgtype(storeID),
		PlatformUserID: platformUserID,
	})
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting customer by platform user: %w", err)
	}

	return r.sqlcToCustomerRow(row), nil
}

// GetByHandle returns a customer by store_id + platform_handle
func (r *Repository) GetByHandle(ctx context.Context, storeID uuid.UUID, handle string) (*CustomerRow, error) {
	row, err := r.queries.GetCustomerByHandle(ctx, sqlc.GetCustomerByHandleParams{
		StoreID:        uuidToPgtype(storeID),
		PlatformHandle: handle,
	})
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting customer by handle: %w", err)
	}

	return r.sqlcToCustomerRow(row), nil
}

// List returns customers with aggregated order stats
func (r *Repository) List(ctx context.Context, params ListCustomersParams) (ListCustomersResult, error) {
	var result ListCustomersResult

	storeUUID, err := uuid.Parse(params.StoreID)
	if err != nil {
		return result, fmt.Errorf("parsing store id: %w", err)
	}

	// Get total count
	count, err := r.queries.CountCustomers(ctx, uuidToPgtype(storeUUID))
	if err != nil {
		return result, fmt.Errorf("counting customers: %w", err)
	}
	result.Total = int(count)

	// Pagination
	limit := int32(params.Pagination.Limit)
	if limit <= 0 {
		limit = 20
	}
	offset := int32((params.Pagination.Page - 1) * params.Pagination.Limit)
	if offset < 0 {
		offset = 0
	}

	var rows []sqlc.ListCustomersRow

	// Use search if provided
	if params.Search != "" {
		searchPattern := "%" + params.Search + "%"
		searchRows, err := r.queries.SearchCustomers(ctx, sqlc.SearchCustomersParams{
			StoreID:        uuidToPgtype(storeUUID),
			PlatformHandle: searchPattern,
			Limit:          limit,
			Offset:         offset,
		})
		if err != nil {
			return result, fmt.Errorf("searching customers: %w", err)
		}
		// Convert search rows to list rows
		for _, sr := range searchRows {
			rows = append(rows, sqlc.ListCustomersRow{
				ID:             sr.ID,
				StoreID:        sr.StoreID,
				PlatformUserID: sr.PlatformUserID,
				PlatformHandle: sr.PlatformHandle,
				Email:          sr.Email,
				Phone:          sr.Phone,
				FirstOrderAt:   sr.FirstOrderAt,
				LastOrderAt:    sr.LastOrderAt,
				CreatedAt:      sr.CreatedAt,
				UpdatedAt:      sr.UpdatedAt,
				TotalOrders:    sr.TotalOrders,
				TotalSpent:     sr.TotalSpent,
			})
		}
	} else {
		rows, err = r.queries.ListCustomers(ctx, sqlc.ListCustomersParams{
			StoreID: uuidToPgtype(storeUUID),
			Limit:   limit,
			Offset:  offset,
		})
		if err != nil {
			return result, fmt.Errorf("listing customers: %w", err)
		}
	}

	result.Customers = make([]CustomerWithStatsRow, len(rows))
	for i, row := range rows {
		result.Customers[i] = CustomerWithStatsRow{
			ID:             pgtypeToUUID(row.ID).String(),
			PlatformUserID: row.PlatformUserID,
			Handle:         row.PlatformHandle,
			Email:          pgtypeTextToPtr(row.Email),
			Phone:          pgtypeTextToPtr(row.Phone),
			TotalOrders:    int(row.TotalOrders),
			TotalSpent:     row.TotalSpent,
		}
		if row.LastOrderAt.Valid {
			t := row.LastOrderAt.Time
			result.Customers[i].LastOrderAt = &t
		}
		if row.FirstOrderAt.Valid {
			t := row.FirstOrderAt.Time
			result.Customers[i].FirstOrderAt = &t
		}
	}

	return result, nil
}

// GetStats returns aggregated statistics for customers
func (r *Repository) GetStats(ctx context.Context, storeID string) (*CustomerStatsOutput, error) {
	storeUUID, err := uuid.Parse(storeID)
	if err != nil {
		return nil, fmt.Errorf("parsing store id: %w", err)
	}

	row, err := r.queries.GetCustomerStats(ctx, uuidToPgtype(storeUUID))
	if err != nil {
		return nil, fmt.Errorf("getting customer stats: %w", err)
	}

	return &CustomerStatsOutput{
		TotalCustomers:      int(row.TotalCustomers),
		ActiveCustomers:     int(row.ActiveCustomers),
		AvgSpentPerCustomer: row.AvgSpentPerCustomer,
	}, nil
}

// ListOrders returns the latest carts (paid + pending) for a customer.
func (r *Repository) ListOrders(ctx context.Context, id uuid.UUID, limit, offset int32) ([]CustomerOrderOutput, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.queries.ListCartsByCustomer(ctx, sqlc.ListCartsByCustomerParams{
		CustomerID: uuidToPgtype(id),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		return nil, fmt.Errorf("listing carts by customer: %w", err)
	}

	orders := make([]CustomerOrderOutput, len(rows))
	for i, row := range rows {
		orders[i] = CustomerOrderOutput{
			ID:            pgtypeToUUID(row.ID).String(),
			ShortID:       row.ShortID,
			Status:        row.Status,
			PaymentStatus: pgtypeTextToPtr(row.PaymentStatus),
			TotalItems:    int(row.TotalItems),
			TotalValue:    row.TotalValue,
		}
		if row.PaidAt.Valid {
			t := row.PaidAt.Time
			orders[i].PaidAt = &t
		}
		if row.CreatedAt.Valid {
			t := row.CreatedAt.Time
			orders[i].CreatedAt = &t
		}
	}
	return orders, nil
}

// Update updates customer fields
func (r *Repository) Update(ctx context.Context, id uuid.UUID, input UpdateCustomerInput) error {
	params := sqlc.UpdateCustomerParams{
		ID: uuidToPgtype(id),
	}
	if input.Handle != nil {
		params.PlatformHandle = *input.Handle
	}
	if input.Email != nil {
		params.Email = pgtype.Text{String: *input.Email, Valid: true}
	}
	if input.Phone != nil {
		params.Phone = pgtype.Text{String: *input.Phone, Valid: true}
	}

	err := r.queries.UpdateCustomer(ctx, params)
	if err != nil {
		return fmt.Errorf("updating customer: %w", err)
	}

	return nil
}

// UpdateLastOrder updates the last_order_at timestamp
func (r *Repository) UpdateLastOrder(ctx context.Context, id uuid.UUID) error {
	err := r.queries.UpdateCustomerLastOrder(ctx, uuidToPgtype(id))
	if err != nil {
		return fmt.Errorf("updating customer last order: %w", err)
	}
	return nil
}

// GetCheckoutSnapshot pulls the most-recent non-empty customer/shipping
// fields from any cart owned by the customer. Returns nil when there is no
// cart with checkout data yet.
func (r *Repository) GetCheckoutSnapshot(ctx context.Context, id uuid.UUID) (*CustomerCheckoutSnapshot, error) {
	row, err := r.queries.GetCustomerCheckoutSnapshot(ctx, uuidToPgtype(id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting checkout snapshot: %w", err)
	}

	snap := &CustomerCheckoutSnapshot{}
	if row.CustomerName.Valid {
		snap.Name = row.CustomerName.String
	}
	if row.CustomerDocument.Valid {
		snap.Document = row.CustomerDocument.String
	}
	if row.CustomerPhone.Valid {
		snap.Phone = row.CustomerPhone.String
	}
	if row.CustomerEmail.Valid {
		snap.Email = row.CustomerEmail.String
	}
	if len(row.ShippingAddress) > 0 {
		// shipping_address is JSONB with the same shape the checkout writes;
		// best-effort decode and silently drop malformed payloads.
		var addr CustomerShippingAddress
		if err := json.Unmarshal(row.ShippingAddress, &addr); err == nil {
			if addr.ZipCode != "" || addr.Street != "" || addr.City != "" {
				snap.Address = &addr
			}
		}
	}
	return snap, nil
}

// CustomerCheckoutSnapshot is the merged identity+address blob the service
// uses to enrich the bare customer row.
type CustomerCheckoutSnapshot struct {
	Name     string
	Document string
	Phone    string
	Email    string
	Address  *CustomerShippingAddress
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

func pgtypeTextToPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func (r *Repository) sqlcToCustomerRow(c sqlc.Customer) *CustomerRow {
	row := &CustomerRow{
		ID:             pgtypeToUUID(c.ID).String(),
		PlatformUserID: c.PlatformUserID,
		Handle:         c.PlatformHandle,
	}
	if c.Email.Valid {
		row.Email = &c.Email.String
	}
	if c.Phone.Valid {
		row.Phone = &c.Phone.String
	}
	if c.LastOrderAt.Valid {
		row.LastOrderAt = &c.LastOrderAt.Time
	}
	if c.FirstOrderAt.Valid {
		row.FirstOrderAt = &c.FirstOrderAt.Time
	}
	return row
}

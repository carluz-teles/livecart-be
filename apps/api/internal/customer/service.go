package customer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo         *Repository
	logger       *zap.Logger
	cartCanceler CartCanceler
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("customer"),
	}
}

// UpsertForCart is a convenience method for the live cart flow: it parses the
// store UUID, calls Upsert with handle-only data (email/phone are filled in
// later by checkout) and returns the customer's UUID as a string. Satisfies
// the live.CustomerUpserter interface.
func (s *Service) UpsertForCart(ctx context.Context, storeID, platformUserID, platformHandle string) (string, error) {
	storeUUID, err := uuid.Parse(storeID)
	if err != nil {
		return "", fmt.Errorf("parsing store id: %w", err)
	}
	out, err := s.Upsert(ctx, UpsertCustomerInput{
		StoreID:        storeUUID,
		PlatformUserID: platformUserID,
		PlatformHandle: platformHandle,
	})
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

// Upsert creates a new customer or updates existing one (by store_id + platform_user_id)
// Returns the customer (existing or new) with its UUID
func (s *Service) Upsert(ctx context.Context, input UpsertCustomerInput) (*CustomerOutput, error) {
	row, err := s.repo.Upsert(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("upserting customer: %w", err)
	}

	s.logger.Debug("customer upserted",
		zap.String("customerId", row.ID),
		zap.String("storeId", input.StoreID.String()),
		zap.String("platformUserID", input.PlatformUserID),
		zap.String("handle", input.PlatformHandle))

	return &CustomerOutput{
		ID:           row.ID,
		Handle:       row.Handle,
		Email:        row.Email,
		Phone:        row.Phone,
		LastOrderAt:  row.LastOrderAt,
		FirstOrderAt: row.FirstOrderAt,
	}, nil
}

// GetByID returns a customer by its UUID, enriched with cart totals so the
// detail drawer matches the numbers shown on the listing.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*CustomerOutput, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("customer %s not found", id))
	}

	// Aggregate from the customer's carts to keep the detail view consistent
	// with the list aggregation (COUNT distinct carts, SUM line totals).
	orders, err := s.repo.ListOrders(ctx, id, 1000, 0)
	if err != nil {
		return nil, fmt.Errorf("listing customer orders: %w", err)
	}
	var totalSpent int64
	for _, o := range orders {
		totalSpent += o.TotalValue
	}

	return &CustomerOutput{
		ID:           row.ID,
		Handle:       row.Handle,
		Email:        row.Email,
		Phone:        row.Phone,
		TotalOrders:  len(orders),
		TotalSpent:   totalSpent,
		LastOrderAt:  row.LastOrderAt,
		FirstOrderAt: row.FirstOrderAt,
	}, nil
}

// GetByPlatformUser returns a customer by store_id + platform_user_id
func (s *Service) GetByPlatformUser(ctx context.Context, storeID uuid.UUID, platformUserID string) (*CustomerOutput, error) {
	row, err := s.repo.GetByPlatformUser(ctx, storeID, platformUserID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil // Not an error, just doesn't exist
	}

	return &CustomerOutput{
		ID:           row.ID,
		Handle:       row.Handle,
		Email:        row.Email,
		Phone:        row.Phone,
		LastOrderAt:  row.LastOrderAt,
		FirstOrderAt: row.FirstOrderAt,
	}, nil
}

// List returns customers with pagination and search
func (s *Service) List(ctx context.Context, input ListCustomersInput) (ListCustomersOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("last_order_at")

	result, err := s.repo.List(ctx, ListCustomersParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListCustomersOutput{}, err
	}

	customers := make([]CustomerOutput, len(result.Customers))
	for i, row := range result.Customers {
		customers[i] = CustomerOutput{
			ID:           row.ID,
			Handle:       row.Handle,
			Email:        row.Email,
			Phone:        row.Phone,
			TotalOrders:  row.TotalOrders,
			TotalSpent:   row.TotalSpent,
			LastOrderAt:  row.LastOrderAt,
			FirstOrderAt: row.FirstOrderAt,
		}
	}

	return ListCustomersOutput{
		Customers:  customers,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

// GetStats returns aggregated customer statistics
func (s *Service) GetStats(ctx context.Context, storeID string) (*CustomerStatsOutput, error) {
	stats, err := s.repo.GetStats(ctx, storeID)
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// ListOrders returns recent carts for a customer (paid + pending).
func (s *Service) ListOrders(ctx context.Context, id uuid.UUID, limit, offset int32) ([]CustomerOrderOutput, error) {
	return s.repo.ListOrders(ctx, id, limit, offset)
}

// Update updates customer fields
func (s *Service) Update(ctx context.Context, id uuid.UUID, input UpdateCustomerInput) error {
	return s.repo.Update(ctx, id, input)
}

// UpdateLastOrder updates the last_order_at timestamp (called when cart is created/updated)
func (s *Service) UpdateLastOrder(ctx context.Context, id uuid.UUID) error {
	return s.repo.UpdateLastOrder(ctx, id)
}

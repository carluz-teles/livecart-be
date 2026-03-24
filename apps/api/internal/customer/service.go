package customer

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("customer"),
	}
}

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

func (s *Service) GetByID(ctx context.Context, storeID, customerID string) (*CustomerOutput, error) {
	row, err := s.repo.GetByID(ctx, storeID, customerID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("customer %s not found", customerID))
	}

	return &CustomerOutput{
		ID:           row.ID,
		Handle:       row.Handle,
		TotalOrders:  row.TotalOrders,
		TotalSpent:   row.TotalSpent,
		LastOrderAt:  row.LastOrderAt,
		FirstOrderAt: row.FirstOrderAt,
	}, nil
}

func (s *Service) GetStats(ctx context.Context, storeID string) (*CustomerStatsOutput, error) {
	stats, err := s.repo.GetStats(ctx, storeID)
	if err != nil {
		return nil, err
	}
	return stats, nil
}

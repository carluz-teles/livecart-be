package cart

import (
	"context"

	"go.uber.org/zap"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("cart"),
	}
}

func (s *Service) List(ctx context.Context, input ListCartsInput) (ListCartsOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListCartsParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListCartsOutput{}, err
	}

	carts := make([]CartOutput, len(result.Carts))
	for i, row := range result.Carts {
		carts[i] = CartOutput{
			ID:             row.ID,
			ShortID:        row.ShortID,
			PlatformHandle: row.PlatformHandle,
			CustomerName:   row.CustomerName,
			CustomerPhone:  row.CustomerPhone,
			TotalCents:     row.TotalCents,
			Status:         row.Status,
			ExpiresAt:      row.ExpiresAt,
			CreatedAt:      row.CreatedAt,
		}
	}

	return ListCartsOutput{
		Carts:      carts,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) GetStats(ctx context.Context, storeID string) (*CartStatsOutput, error) {
	row, err := s.repo.GetStats(ctx, storeID)
	if err != nil {
		return nil, err
	}

	var avgTicket int64
	if row.OpenCarts > 0 {
		avgTicket = row.OpenValueCents / int64(row.OpenCarts)
	}

	return &CartStatsOutput{
		OpenCarts:        row.OpenCarts,
		OpenValueCents:   row.OpenValueCents,
		AvgTicketCents:   avgTicket,
		RecoverableCarts: row.RecoverableCarts,
	}, nil
}


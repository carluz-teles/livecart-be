package dashboard

import (
	"context"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) GetStats(ctx context.Context, storeID string) (*DashboardStatsOutput, error) {
	row, err := s.repo.GetStats(ctx, storeID)
	if err != nil {
		return nil, err
	}

	return &DashboardStatsOutput{
		TotalRevenue:   row.TotalRevenue,
		TotalOrders:    row.TotalOrders,
		ActiveProducts: row.ActiveProducts,
		TotalLives:     row.TotalLives,
	}, nil
}

func (s *Service) GetMonthlyRevenue(ctx context.Context, storeID string) (*MonthlyRevenueOutput, error) {
	rows, err := s.repo.GetMonthlyRevenue(ctx, storeID)
	if err != nil {
		return nil, err
	}

	return &MonthlyRevenueOutput{Items: rows}, nil
}

func (s *Service) GetTopProducts(ctx context.Context, storeID string) (*TopProductsOutput, error) {
	rows, err := s.repo.GetTopProducts(ctx, storeID)
	if err != nil {
		return nil, err
	}

	return &TopProductsOutput{Products: rows}, nil
}

package dashboard

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
		logger: logger.Named("dashboard"),
	}
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

// =============================================================================
// ANALYTICS - Revenue Attribution
// =============================================================================

// GetEventsWithRevenue returns all events with their revenue metrics
func (s *Service) GetEventsWithRevenue(ctx context.Context, storeID string, limit int) ([]EventWithRevenueOutput, error) {
	rows, err := s.repo.GetEventsWithRevenue(ctx, storeID, limit)
	if err != nil {
		return nil, err
	}

	events := make([]EventWithRevenueOutput, len(rows))
	for i, row := range rows {
		events[i] = EventWithRevenueOutput{
			ID:               row.ID,
			Title:            row.Title,
			Status:           row.Status,
			CreatedAt:        row.CreatedAt,
			TotalComments:    row.TotalComments,
			TotalCarts:       row.TotalCarts,
			PaidCarts:        row.PaidCarts,
			ConfirmedRevenue: row.ConfirmedRevenue,
		}
	}

	return events, nil
}

// GetAggregatedFunnel returns aggregated funnel metrics for the store
func (s *Service) GetAggregatedFunnel(ctx context.Context, storeID string, days int) (*AggregatedFunnelOutput, error) {
	row, err := s.repo.GetAggregatedFunnel(ctx, storeID, days)
	if err != nil {
		return nil, err
	}

	return &AggregatedFunnelOutput{
		TotalComments:    row.TotalComments,
		TotalCarts:       row.TotalCarts,
		CheckoutCarts:    row.CheckoutCarts,
		PaidCarts:        row.PaidCarts,
		ConfirmedRevenue: row.ConfirmedRevenue,
		AverageTicket:    row.AverageTicket,
	}, nil
}

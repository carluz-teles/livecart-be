package live

import (
	"context"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, input CreateLiveInput) (CreateLiveOutput, error) {
	row, err := s.repo.Create(ctx, CreateLiveParams{
		StoreID:        input.StoreID,
		Title:          input.Title,
		Platform:       input.Platform,
		PlatformLiveID: input.PlatformLiveID,
		Status:         "scheduled",
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	return CreateLiveOutput{
		ID:        row.ID,
		Title:     row.Title,
		Platform:  row.Platform,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id, storeID string) (LiveOutput, error) {
	row, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}
	return toLiveOutput(*row), nil
}

func (s *Service) List(ctx context.Context, input ListLivesInput) (ListLivesOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListLivesParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListLivesOutput{}, err
	}

	lives := make([]LiveOutput, len(result.Lives))
	for i, row := range result.Lives {
		lives[i] = toLiveOutput(row)
	}

	return ListLivesOutput{
		Lives:      lives,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateLiveInput) (LiveOutput, error) {
	row, err := s.repo.Update(ctx, UpdateLiveParams{
		ID:             input.ID,
		StoreID:        input.StoreID,
		Title:          input.Title,
		Platform:       input.Platform,
		PlatformLiveID: input.PlatformLiveID,
	})
	if err != nil {
		return LiveOutput{}, err
	}

	return toLiveOutput(row), nil
}

func (s *Service) Start(ctx context.Context, id, storeID string) (LiveOutput, error) {
	row, err := s.repo.Start(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}
	return toLiveOutput(row), nil
}

func (s *Service) End(ctx context.Context, id, storeID string) (LiveOutput, error) {
	row, err := s.repo.End(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}
	return toLiveOutput(row), nil
}

func (s *Service) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

func toLiveOutput(row LiveRow) LiveOutput {
	return LiveOutput{
		ID:             row.ID,
		Title:          row.Title,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		Status:         row.Status,
		StartedAt:      row.StartedAt,
		EndedAt:        row.EndedAt,
		TotalComments:  row.TotalComments,
		TotalOrders:    row.TotalOrders,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

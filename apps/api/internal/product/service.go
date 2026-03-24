package product

import (
	"context"
	"fmt"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, input CreateProductInput) (CreateProductOutput, error) {
	// Resolve keyword: validate if provided, or auto-generate
	keyword, err := s.resolveKeyword(ctx, input.StoreID, input.Keyword)
	if err != nil {
		return CreateProductOutput{}, err
	}

	// Check uniqueness
	existing, err := s.repo.GetByKeyword(ctx, GetByKeywordParams{
		StoreID: input.StoreID,
		Keyword: keyword.String(),
	})
	if err != nil && !httpx.IsNotFound(err) {
		return CreateProductOutput{}, fmt.Errorf("checking keyword uniqueness: %w", err)
	}
	if existing != nil {
		return CreateProductOutput{}, httpx.ErrConflict("keyword already in use")
	}

	row, err := s.repo.Create(ctx, CreateProductParams{
		StoreID:        input.StoreID,
		Name:           input.Name,
		ExternalID:     input.ExternalID,
		ExternalSource: input.ExternalSource,
		Keyword:        keyword.String(),
		Price:          input.Price,
		ImageURL:       input.ImageURL,
		Stock:          input.Stock,
	})
	if err != nil {
		return CreateProductOutput{}, fmt.Errorf("creating product: %w", err)
	}

	return CreateProductOutput{
		ID:        row.ID,
		Name:      row.Name,
		Keyword:   row.Keyword,
		CreatedAt: row.CreatedAt,
	}, nil
}

// resolveKeyword validates or auto-generates a keyword for a product.
func (s *Service) resolveKeyword(ctx context.Context, storeID, inputKeyword string) (Keyword, error) {
	if inputKeyword != "" {
		kw, err := NewKeyword(inputKeyword)
		if err != nil {
			return Keyword{}, httpx.ErrUnprocessable(fmt.Sprintf("invalid keyword: %s", err.Error()))
		}
		return kw, nil
	}

	// Auto-generate keyword
	maxKw, err := s.repo.GetMaxKeyword(ctx, storeID)
	if err != nil {
		return Keyword{}, fmt.Errorf("getting max keyword: %w", err)
	}

	kw, err := NextKeyword(maxKw)
	if err != nil {
		return Keyword{}, httpx.ErrUnprocessable("keyword range exhausted (max 9999)")
	}

	return kw, nil
}

func (s *Service) GetByID(ctx context.Context, id, storeID string) (ProductOutput, error) {
	row, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return ProductOutput{}, err
	}
	return toProductOutput(*row), nil
}

func (s *Service) List(ctx context.Context, input ListProductsInput) (ListProductsOutput, error) {
	// Normalize pagination and sorting
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListProductsParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListProductsOutput{}, err
	}

	products := make([]ProductOutput, len(result.Products))
	for i, row := range result.Products {
		products[i] = toProductOutput(row)
	}

	return ListProductsOutput{
		Products:   products,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateProductInput) (ProductOutput, error) {
	row, err := s.repo.Update(ctx, UpdateProductParams{
		ID:       input.ID,
		StoreID:  input.StoreID,
		Name:     input.Name,
		Price:    input.Price,
		ImageURL: input.ImageURL,
		Stock:    input.Stock,
		Active:   input.Active,
	})
	if err != nil {
		return ProductOutput{}, err
	}

	return toProductOutput(row), nil
}

func toProductOutput(row ProductRow) ProductOutput {
	return ProductOutput{
		ID:             row.ID,
		Name:           row.Name,
		ExternalID:     row.ExternalID,
		ExternalSource: row.ExternalSource,
		Keyword:        row.Keyword,
		Price:          row.Price,
		ImageURL:       row.ImageURL,
		Stock:          row.Stock,
		Active:         row.Active,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func (s *Service) GetStats(ctx context.Context, storeID string) (ProductStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

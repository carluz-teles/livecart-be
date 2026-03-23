package product

import (
	"context"
	"fmt"
	"strconv"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, input CreateProductInput) (CreateProductOutput, error) {
	keyword := input.Keyword

	// Auto-generate keyword if not provided
	if keyword == "" {
		maxKw, err := s.repo.GetMaxKeyword(ctx, input.StoreID)
		if err != nil {
			return CreateProductOutput{}, fmt.Errorf("getting max keyword: %w", err)
		}
		next, err := nextKeyword(maxKw)
		if err != nil {
			return CreateProductOutput{}, httpx.ErrUnprocessable("keyword range exhausted (max 9999)")
		}
		keyword = next
	} else {
		if !isValidKeyword(keyword) {
			return CreateProductOutput{}, httpx.ErrUnprocessable("keyword must be between 1000 and 9999")
		}
	}

	existing, err := s.repo.GetByKeyword(ctx, GetByKeywordParams{
		StoreID: input.StoreID,
		Keyword: keyword,
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
		Keyword:        keyword,
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

func isValidKeyword(kw string) bool {
	n, err := strconv.Atoi(kw)
	if err != nil {
		return false
	}
	return n >= 1000 && n <= 9999
}

func (s *Service) GetStats(ctx context.Context, storeID string) (ProductStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

func nextKeyword(current string) (string, error) {
	n, err := strconv.Atoi(current)
	if err != nil {
		return "", fmt.Errorf("parsing keyword: %w", err)
	}
	next := n + 1
	if next < 1000 {
		next = 1000
	}
	if next > 9999 {
		return "", fmt.Errorf("keyword range exhausted")
	}
	return fmt.Sprintf("%04d", next), nil
}

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
		Sizes:          input.Sizes,
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

func (s *Service) List(ctx context.Context, storeID string) ([]ProductOutput, error) {
	rows, err := s.repo.ListByStore(ctx, storeID)
	if err != nil {
		return nil, err
	}

	result := make([]ProductOutput, len(rows))
	for i, row := range rows {
		result[i] = toProductOutput(row)
	}
	return result, nil
}

func (s *Service) Update(ctx context.Context, input UpdateProductInput) (ProductOutput, error) {
	row, err := s.repo.Update(ctx, UpdateProductParams{
		ID:       input.ID,
		StoreID:  input.StoreID,
		Name:     input.Name,
		Price:    input.Price,
		ImageURL: input.ImageURL,
		Sizes:    input.Sizes,
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
		Sizes:          row.Sizes,
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

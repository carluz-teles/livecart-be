package product

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("product"),
	}
}

func (s *Service) Create(ctx context.Context, input CreateProductInput) (CreateProductOutput, error) {
	// Resolve keyword: validate if provided, or auto-generate
	keyword, err := s.resolveKeyword(ctx, input.StoreID, input.Keyword)
	if err != nil {
		return CreateProductOutput{}, err
	}

	// Check uniqueness
	existing, err := s.repo.GetByKeyword(ctx, input.StoreID, keyword)
	if err != nil && !httpx.IsNotFound(err) {
		return CreateProductOutput{}, fmt.Errorf("checking keyword uniqueness: %w", err)
	}
	if existing != nil {
		return CreateProductOutput{}, httpx.ErrConflict("keyword already in use")
	}

	// Create product via domain factory
	product, err := domain.NewProduct(
		input.StoreID,
		input.Name,
		input.ExternalID,
		input.ExternalSource,
		keyword,
		input.Price,
		input.ImageURL,
		input.Stock,
	)
	if err != nil {
		return CreateProductOutput{}, fmt.Errorf("creating product: %w", err)
	}

	// Save to repository
	if err := s.repo.Save(ctx, product); err != nil {
		return CreateProductOutput{}, err
	}

	return CreateProductOutput{
		ID:        product.ID().String(),
		Name:      product.Name(),
		Keyword:   product.Keyword().String(),
		CreatedAt: product.CreatedAt(),
	}, nil
}

// resolveKeyword validates or auto-generates a keyword for a product.
func (s *Service) resolveKeyword(ctx context.Context, storeID vo.StoreID, inputKeyword string) (domain.Keyword, error) {
	if inputKeyword != "" {
		kw, err := domain.NewKeyword(inputKeyword)
		if err != nil {
			return domain.Keyword{}, httpx.ErrUnprocessable(fmt.Sprintf("invalid keyword: %s", err.Error()))
		}
		return kw, nil
	}

	// Auto-generate keyword
	maxKw, err := s.repo.GetMaxKeyword(ctx, storeID)
	if err != nil {
		return domain.Keyword{}, fmt.Errorf("getting max keyword: %w", err)
	}

	kw, err := domain.NextKeyword(maxKw)
	if err != nil {
		return domain.Keyword{}, httpx.ErrUnprocessable("keyword range exhausted (max 9999)")
	}

	return kw, nil
}

func (s *Service) GetByID(ctx context.Context, id vo.ProductID, storeID vo.StoreID) (ProductOutput, error) {
	product, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return ProductOutput{}, err
	}
	return toProductOutput(product), nil
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
	for i, product := range result.Products {
		products[i] = toProductOutput(product)
	}

	return ListProductsOutput{
		Products:   products,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateProductInput) (ProductOutput, error) {
	// Get existing product
	product, err := s.repo.GetByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return ProductOutput{}, err
	}

	// Use domain method to update
	if err := product.UpdateDetails(input.Name, input.Price, input.ImageURL, input.Stock, input.Active); err != nil {
		return ProductOutput{}, httpx.ErrUnprocessable(err.Error())
	}

	// Save changes
	if err := s.repo.Update(ctx, product); err != nil {
		return ProductOutput{}, err
	}

	return toProductOutput(product), nil
}

func (s *Service) Delete(ctx context.Context, id vo.ProductID, storeID vo.StoreID) error {
	return s.repo.Delete(ctx, id, storeID)
}

func (s *Service) GetStats(ctx context.Context, storeID vo.StoreID) (ProductStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

func toProductOutput(product *domain.Product) ProductOutput {
	return ProductOutput{
		ID:             product.ID().String(),
		Name:           product.Name(),
		ExternalID:     product.ExternalID(),
		ExternalSource: product.ExternalSource().String(),
		Keyword:        product.Keyword().String(),
		Price:          product.Price().Cents(),
		ImageURL:       product.ImageURL(),
		Stock:          product.Stock(),
		Active:         product.Active(),
		CreatedAt:      product.CreatedAt(),
		UpdatedAt:      product.UpdatedAt(),
	}
}

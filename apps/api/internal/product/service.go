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
		input.Shipping,
	)
	if err != nil {
		return CreateProductOutput{}, fmt.Errorf("creating product: %w", err)
	}

	if input.GroupID != nil {
		product.AttachGroup(input.GroupID)
	}

	// Save to repository
	if err := s.repo.Save(ctx, product); err != nil {
		return CreateProductOutput{}, err
	}

	for i, url := range input.Images {
		if _, err := s.repo.AddImage(ctx, product.ID(), url, i); err != nil {
			return CreateProductOutput{}, fmt.Errorf("attaching variant image: %w", err)
		}
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
	out := toProductOutput(product)

	images, err := s.repo.ListImages(ctx, id)
	if err != nil {
		return ProductOutput{}, err
	}
	out.Images = images

	if product.GroupID() != nil {
		opts, err := s.repo.ListVariantOptions(ctx, id)
		if err != nil {
			return ProductOutput{}, err
		}
		refs := make([]OptionValueRef, len(opts))
		for i, o := range opts {
			refs[i] = OptionValueRef{Option: o.OptionName, Value: o.Value}
		}
		out.OptionValues = refs
	}
	return out, nil
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
	if err := product.UpdateDetails(input.Name, input.Price, input.ImageURL, input.Stock, input.Active, input.Shipping); err != nil {
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

// AddImage attaches one image URL to a variant gallery (after asserting ownership).
func (s *Service) AddImage(ctx context.Context, id vo.ProductID, storeID vo.StoreID, url string, position int) (string, error) {
	if _, err := s.repo.GetByID(ctx, id, storeID); err != nil {
		return "", err
	}
	return s.repo.AddImage(ctx, id, url, position)
}

// DeleteImage removes one image from a variant gallery.
func (s *Service) DeleteImage(ctx context.Context, id vo.ProductID, storeID vo.StoreID, imageID vo.ID) error {
	if _, err := s.repo.GetByID(ctx, id, storeID); err != nil {
		return err
	}
	return s.repo.DeleteImage(ctx, id, imageID)
}

// HasProductByExternalID checks if a product with the given external ID exists.
func (s *Service) HasProductByExternalID(ctx context.Context, storeID vo.StoreID, externalSource domain.ExternalSource, externalID string) (bool, error) {
	existing, err := s.repo.GetByExternalID(ctx, storeID, externalSource, externalID)
	if err != nil {
		return false, err
	}
	return existing != nil, nil
}

// SyncFromERP updates an existing product from ERP data.
// Returns (true, nil) if updated, (false, nil) if product not found in LiveCart.
func (s *Service) SyncFromERP(ctx context.Context, input SyncFromERPInput) (bool, error) {
	existing, err := s.repo.GetByExternalID(ctx, input.StoreID, input.ExternalSource, input.ExternalID)
	if err != nil {
		return false, fmt.Errorf("looking up product by external ID: %w", err)
	}

	if existing == nil {
		return false, nil
	}

	stock := input.Stock
	if input.SkipStock {
		stock = existing.Stock()
	}

	shipping := existing.Shipping()
	if input.Shipping != nil {
		shipping = *input.Shipping
	}

	if err := existing.UpdateDetails(input.Name, input.Price, input.ImageURL, stock, input.Active, shipping); err != nil {
		return false, fmt.Errorf("updating product details: %w", err)
	}
	if err := s.repo.Update(ctx, existing); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) GetStats(ctx context.Context, storeID vo.StoreID) (ProductStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

func toProductOutput(product *domain.Product) ProductOutput {
	groupID := ""
	if g := product.GroupID(); g != nil {
		groupID = g.String()
	}
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
		Shipping:       product.Shipping(),
		Shippable:      product.IsShippable(),
		GroupID:        groupID,
		OptionValues:   []OptionValueRef{},
		Images:         []string{},
		CreatedAt:      product.CreatedAt(),
		UpdatedAt:      product.UpdatedAt(),
	}
}

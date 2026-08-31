package catalog

import (
	"context"

	"go.uber.org/zap"

	vo "livecart/apps/api/lib/valueobject"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("catalog"),
	}
}

// Create builds a catalog and, when given, seeds its product membership.
func (s *Service) Create(ctx context.Context, storeID vo.StoreID, name string, productIDs []vo.ProductID) (Catalog, error) {
	cat, err := s.repo.Create(ctx, storeID, name)
	if err != nil {
		return Catalog{}, err
	}
	if len(productIDs) > 0 {
		catID, err := vo.NewID(cat.ID)
		if err != nil {
			return Catalog{}, err
		}
		if err := s.repo.SetProducts(ctx, catID, productIDs); err != nil {
			return Catalog{}, err
		}
		cat.ProductCount = len(productIDs)
	}
	return cat, nil
}

func (s *Service) GetByID(ctx context.Context, id vo.ID, storeID vo.StoreID) (Catalog, []CatalogProductView, error) {
	cat, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return Catalog{}, nil, err
	}
	products, err := s.repo.ListProducts(ctx, id, storeID)
	if err != nil {
		return Catalog{}, nil, err
	}
	cat.ProductCount = len(products)
	return cat, products, nil
}

func (s *Service) List(ctx context.Context, storeID vo.StoreID) ([]Catalog, error) {
	return s.repo.List(ctx, storeID)
}

func (s *Service) Update(ctx context.Context, id vo.ID, storeID vo.StoreID, name string) (Catalog, error) {
	return s.repo.Update(ctx, id, storeID, name)
}

func (s *Service) Delete(ctx context.Context, id vo.ID, storeID vo.StoreID) error {
	return s.repo.Delete(ctx, id, storeID)
}

// SetProducts replaces the catalog membership after confirming the catalog exists
// for the store (so cross-store writes are rejected).
func (s *Service) SetProducts(ctx context.Context, id vo.ID, storeID vo.StoreID, productIDs []vo.ProductID) ([]CatalogProductView, error) {
	if _, err := s.repo.GetByID(ctx, id, storeID); err != nil {
		return nil, err
	}
	if err := s.repo.SetProducts(ctx, id, productIDs); err != nil {
		return nil, err
	}
	return s.repo.ListProducts(ctx, id, storeID)
}

// SetEventCatalog associates or clears the catalog of an event. When associating,
// the catalog is verified to belong to the store first.
func (s *Service) SetEventCatalog(ctx context.Context, eventID vo.ID, storeID vo.StoreID, catalogID *vo.ID) error {
	if catalogID != nil {
		if _, err := s.repo.GetByID(ctx, *catalogID, storeID); err != nil {
			return err
		}
	}
	return s.repo.SetEventCatalog(ctx, eventID, storeID, catalogID)
}

func (s *Service) GetEventCatalog(ctx context.Context, eventID vo.ID, storeID vo.StoreID) (Catalog, error) {
	return s.repo.GetEventCatalog(ctx, eventID, storeID)
}

func (s *Service) GetPublicCatalogByEvent(ctx context.Context, eventID vo.ID) (Catalog, []CatalogProductView, error) {
	return s.repo.GetPublicCatalogByEvent(ctx, eventID)
}

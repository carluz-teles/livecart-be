package product

import (
	"context"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product/domain"
	vo "livecart/apps/api/lib/valueobject"
)

// ProductSyncerAdapter adapts the product Service for use by the integration package.
type ProductSyncerAdapter struct {
	service *Service
}

// NewProductSyncerAdapter creates a new adapter.
func NewProductSyncerAdapter(service *Service) *ProductSyncerAdapter {
	return &ProductSyncerAdapter{service: service}
}

// HasProduct checks if a product with the given external ID exists in LiveCart.
func (a *ProductSyncerAdapter) HasProduct(ctx context.Context, storeID, externalID, externalSource string) (bool, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return false, err
	}

	es, err := domain.NewExternalSource(externalSource)
	if err != nil {
		return false, err
	}

	return a.service.HasProductByExternalID(ctx, sid, es, externalID)
}

// GetProduct returns the external ID and source for a product registered in LiveCart.
func (a *ProductSyncerAdapter) GetProduct(ctx context.Context, storeID, productID string) (string, string, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return "", "", err
	}

	pid, err := vo.NewProductID(productID)
	if err != nil {
		return "", "", err
	}

	output, err := a.service.GetByID(ctx, pid, sid)
	if err != nil {
		return "", "", err
	}

	return output.ExternalID, output.ExternalSource, nil
}

// ImportProduct creates a new simple product in LiveCart from an ERP product.
// Returns the LiveCart product UUID.
func (a *ProductSyncerAdapter) ImportProduct(ctx context.Context, storeID, externalSource string, p providers.ERPProduct) (string, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return "", err
	}
	es, err := domain.NewExternalSource(externalSource)
	if err != nil {
		return "", err
	}
	money, err := vo.NewMoney(p.Price)
	if err != nil {
		return "", err
	}

	out, err := a.service.Create(ctx, CreateProductInput{
		StoreID:        sid,
		Name:           p.Name,
		ExternalID:     p.ID,
		ExternalSource: es,
		Price:          money,
		ImageURL:       p.ImageURL,
		Stock:          p.Stock,
		Shipping:       erpShippingToDomain(p.Shipping),
	})
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

// erpShippingToDomain maps the ERP-side shipping profile to the LiveCart
// domain shape. Returns an empty profile when the ERP did not supply one
// (callers should treat empty as "merchant fills later via PUT").
func erpShippingToDomain(s *providers.ERPShippingProfile) domain.ShippingProfile {
	if s == nil {
		return domain.ShippingProfile{}
	}
	w := s.WeightGrams
	h := s.HeightCm
	wd := s.WidthCm
	l := s.LengthCm
	pf, _ := domain.NewPackageFormat(s.PackageFormat)
	return domain.ShippingProfile{
		WeightGrams:   &w,
		HeightCm:      &h,
		WidthCm:       &wd,
		LengthCm:      &l,
		PackageFormat: pf,
	}
}

// SyncProduct updates a product from an ERP webhook notification.
func (a *ProductSyncerAdapter) SyncProduct(ctx context.Context, storeID, externalID, externalSource, name string, price int64, imageURL string, stock int, active bool, skipStock bool) error {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return err
	}

	es, err := domain.NewExternalSource(externalSource)
	if err != nil {
		return err
	}

	money, err := vo.NewMoney(price)
	if err != nil {
		return err
	}

	_, err = a.service.SyncFromERP(ctx, SyncFromERPInput{
		StoreID:        sid,
		ExternalID:     externalID,
		ExternalSource: es,
		Name:           name,
		Price:          money,
		ImageURL:       imageURL,
		Stock:          stock,
		Active:         active,
		SkipStock:      skipStock,
	})
	return err
}

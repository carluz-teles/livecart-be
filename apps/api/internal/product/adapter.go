package product

import (
	"context"

	"livecart/apps/api/internal/product/domain"
	vo "livecart/apps/api/lib/valueobject"
)

// ProductSyncerAdapter adapts the product Service for use by the integration package.
type ProductSyncerAdapter struct {
	service        *Service
	errNotRegistered error
}

// NewProductSyncerAdapter creates a new adapter.
// errNotRegistered is the error to return when the product doesn't exist in LiveCart.
func NewProductSyncerAdapter(service *Service, errNotRegistered error) *ProductSyncerAdapter {
	return &ProductSyncerAdapter{service: service, errNotRegistered: errNotRegistered}
}

// SyncProduct updates a product from an ERP webhook notification.
// Returns errNotRegistered if the product is not registered in LiveCart.
func (a *ProductSyncerAdapter) SyncProduct(ctx context.Context, storeID, externalID, externalSource, name string, price int64, imageURL string, stock int, active bool) error {
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

	updated, err := a.service.SyncFromERP(ctx, SyncFromERPInput{
		StoreID:        sid,
		ExternalID:     externalID,
		ExternalSource: es,
		Name:           name,
		Price:          money,
		ImageURL:       imageURL,
		Stock:          stock,
		Active:         active,
	})
	if err != nil {
		return err
	}

	if !updated {
		return a.errNotRegistered
	}

	return nil
}

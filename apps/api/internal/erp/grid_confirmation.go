package erp

import (
	"context"
	"errors"

	"livecart/apps/api/internal/integration/providers"
)

var ErrOrderBusy = errors.New("ERP order has an operation in progress")

// Confirmation refers to the exact grid accepted by the provider. A later
// addition must keep its pending marker even when an earlier write succeeds.
type ERPGridAcknowledger interface {
	ConfirmERPGrid(context.Context, string, []providers.ERPOrderItem) error
}

// Explicit merchant removals identify legacy unmarked lines as owned by LiveCart.
type editedProductsKey struct{}

func WithEditedProducts(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, editedProductsKey{}, ids)
}
func EditedProducts(ctx context.Context) []string {
	ids, _ := ctx.Value(editedProductsKey{}).([]string)
	return ids
}

// PendingERPGridOwnership is also read by the existing operation sweep, which
// can recover a crash before the merchant queue's lease expires.
type PendingERPGridOwnership interface {
	PendingERPGridProducts(context.Context, string) ([]string, error)
}

func (s *Service) withPendingERPGridOwnership(ctx context.Context, cartID string) (context.Context, error) {
	if ctx.Value(editedProductsKey{}) != nil {
		return ctx, nil
	}
	reader, ok := s.repo.(PendingERPGridOwnership)
	if !ok {
		return ctx, nil
	}
	ids, err := reader.PendingERPGridProducts(ctx, cartID)
	if err != nil {
		return ctx, err
	}
	if ids == nil {
		ids = []string{}
	}
	return WithEditedProducts(ctx, ids), nil
}

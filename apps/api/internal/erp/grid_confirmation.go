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

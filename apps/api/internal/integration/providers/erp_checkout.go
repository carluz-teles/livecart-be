package providers

import "context"

// ERPOrderCheckout is the commercial snapshot of a paid checkout. Freight is
// what the buyer paid; Shipping.CostCents is the merchant's carrier expense.
type ERPOrderCheckout struct {
	Customer      ERPContactInput
	FreightCents  int64
	DiscountCents int64
	Shipping      *ERPOrderShipping
	Address       *ERPShippingAddress
	Payments      []ERPInstallment
}

// ERPOrderCheckoutSyncer applies the commercial snapshot and payment ledger
// together, before approval. Providers without this capability retain their
// existing confirmation flow.
type ERPOrderCheckoutSyncer interface {
	SyncOrderCheckout(context.Context, string, ERPOrderCheckout) error
}

package providers

import "context"

// ERPApprovalSnapshot is one read of the sale, not a request to approve it.
type ERPApprovalSnapshot struct {
	OrderID      string
	Status       ERPOrderStatus
	Items        []ERPOrderItem
	TotalCents   int64
	FreightCents int64
	InvoiceID    string
}

type ERPApprovalReader interface {
	GetOrderApprovalSnapshot(context.Context, string) (*ERPApprovalSnapshot, error)
}

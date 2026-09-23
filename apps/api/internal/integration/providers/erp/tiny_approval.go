package erp

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"livecart/apps/api/internal/integration/providers"
)

// Reuse the checkout GET so status, value and items come from the same response.
// This path performs no payment, stock, contact or order write in Tiny.
func (t *Tiny) GetOrderApprovalSnapshot(ctx context.Context, orderID string) (*providers.ERPApprovalSnapshot, error) {
	order, err := t.readCheckoutOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	status, known := providers.ERPOrderStatusFromSituacao(order.Status)
	if !known || strconv.FormatInt(order.ID, 10) != orderID {
		return nil, fmt.Errorf("Tiny approval returned an unknown situation or a different order")
	}
	out := &providers.ERPApprovalSnapshot{
		OrderID: orderID, Status: status, Items: []providers.ERPOrderItem{},
		TotalCents:   int64(math.Round(order.Total * 100)),
		FreightCents: int64(math.Round(order.Freight * 100)),
	}
	if order.InvoiceID > 0 {
		out.InvoiceID = strconv.FormatInt(order.InvoiceID, 10)
	}
	for _, item := range order.Items {
		if item.Product.ID <= 0 || item.Quantity <= 0 || item.UnitPrice < 0 {
			return nil, fmt.Errorf("Tiny approval returned an invalid order item")
		}
		out.Items = append(out.Items, providers.ERPOrderItem{
			ProductID: strconv.FormatInt(item.Product.ID, 10), Quantity: item.Quantity,
			UnitPrice: int64(math.Round(item.UnitPrice * 100)), Note: item.Note,
		})
	}
	if out.TotalCents <= 0 || out.FreightCents < 0 || len(out.Items) == 0 {
		return nil, fmt.Errorf("Tiny approval returned an incomplete sale")
	}
	return out, nil
}

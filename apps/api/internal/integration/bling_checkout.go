package integration

import (
	"context"
	"encoding/json"
	"fmt"

	"livecart/apps/api/internal/integration/providers"
)

// LoadERPOrderCheckout loads the paid commercial snapshot under the cart's
// store boundary. It is used only by providers implementing checkout sync.
func (s *Service) LoadERPOrderCheckout(ctx context.Context, cartID, storeID string) (providers.ERPOrderCheckout, error) {
	var out providers.ERPOrderCheckout
	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return out, fmt.Errorf("loading checkout cart: %w", err)
	}
	if cart.StoreID != storeID {
		return out, fmt.Errorf("checkout cart does not belong to store")
	}
	cID, err := parseUUID(cartID)
	if err != nil {
		return out, err
	}
	var service, carrier string
	var realCost int64
	var deadline int
	// Unlike the legacy best-effort shipping lookup, failure here blocks
	// approval; interpreting unavailable data as zero would erase freight.
	if err := s.repo.pool.QueryRow(ctx, `
		SELECT COALESCE(shipping_cost_cents, 0),
		       COALESCE(shipping_service_name, ''), COALESCE(shipping_carrier, ''),
		       COALESCE(shipping_cost_real_cents, 0), COALESCE(shipping_deadline_days, 0)
		FROM carts WHERE id = $1
	`, cID).Scan(&out.FreightCents, &service, &carrier, &realCost, &deadline); err != nil {
		return out, fmt.Errorf("loading paid checkout freight: %w", err)
	}
	if service != "" || carrier != "" {
		out.Shipping = &providers.ERPOrderShipping{
			Service: service, Carrier: carrier, CostCents: realCost, DeadlineDays: deadline,
		}
	}
	if len(cart.ShippingAddress) > 0 && string(cart.ShippingAddress) != "null" && carrier != providers.StorePickupCarrier {
		var address struct {
			ZipCode      string `json:"zipCode"`
			Street       string `json:"street"`
			Number       string `json:"number"`
			Complement   string `json:"complement"`
			Neighborhood string `json:"neighborhood"`
			City         string `json:"city"`
			State        string `json:"state"`
		}
		if err := json.Unmarshal(cart.ShippingAddress, &address); err != nil {
			return out, fmt.Errorf("reading paid checkout delivery address: %w", err)
		}
		if address.Street != "" {
			out.Address = &providers.ERPShippingAddress{
				RecipientName: cart.CustomerName, Document: cart.CustomerDocument,
				Phone: cart.CustomerPhone, Street: address.Street, Number: address.Number,
				Complement: address.Complement, Neighborhood: address.Neighborhood,
				City: address.City, State: address.State, ZipCode: address.ZipCode,
			}
		}
	}
	items, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
	if err != nil {
		return out, fmt.Errorf("loading checkout items: %w", err)
	}
	for _, item := range items {
		if item.Quantity > 0 && item.ProductExternalID == "" {
			return out, fmt.Errorf("paid checkout contains items without ERP linkage; automatic payment allocation requires reconciliation")
		}
	}
	payments, err := s.repo.ListCartPayments(ctx, cartID)
	if err != nil {
		return out, fmt.Errorf("loading checkout payment ledger: %w", err)
	}
	if len(payments) == 0 {
		return out, fmt.Errorf("paid checkout has no payment ledger; automatic confirmation requires reconciliation")
	}
	for _, payment := range payments {
		if payment.GrossCoveredCents < payment.AmountCents {
			return out, fmt.Errorf("checkout payment exceeds the gross amount it covers")
		}
		out.DiscountCents += payment.GrossCoveredCents - payment.AmountCents
		out.Payments = append(out.Payments, providers.ERPInstallment{
			AmountCents: payment.AmountCents, DueDate: payment.PaidAt, Method: payment.Method,
			Note: "PAGO LiveCart — " + payment.Method + " " + payment.CheckoutID,
		})
	}
	return out, nil
}

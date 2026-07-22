package customer

import (
	"time"

	"github.com/google/uuid"

	"livecart/apps/api/internal/customer/domain"
	"livecart/apps/api/lib/query"
)

// ============================================
// Handler layer - Filters
// ============================================

type CustomerFilters struct {
	HasOrders     *bool `query:"hasOrders"`
	OrderCountMin *int  `query:"orderCountMin"`
	OrderCountMax *int  `query:"orderCountMax"`
	TotalSpentMin *int  `query:"totalSpentMin"`
	TotalSpentMax *int  `query:"totalSpentMax"`
}

// ============================================
// Handler layer - Response types
// ============================================

// CustomerShippingAddressResponse mirrors the JSON the buyer fills at checkout
// (zipCode, street, number, complement, neighborhood, city, state). Sent
// verbatim by the detail endpoint so the FE can render the last delivery
// destination without re-fetching an order.
type CustomerShippingAddressResponse struct {
	ZipCode      string `json:"zipCode,omitempty"`
	Street       string `json:"street,omitempty"`
	Number       string `json:"number,omitempty"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
}

type CustomerResponse struct {
	ID     string  `json:"id"`
	Handle string  `json:"handle"`
	Email  *string `json:"email,omitempty"`
	Phone  *string `json:"phone,omitempty"`
	// Identity fields captured at the most recent checkout. Empty until the
	// buyer fills the public cart form. Surfaced on the detail drawer so the
	// merchant can address the customer by name and call them via WhatsApp.
	Name         *string    `json:"name,omitempty"`
	Document     *string    `json:"document,omitempty"`
	TotalOrders  int        `json:"totalOrders"`
	TotalSpent   int64      `json:"totalSpent"`
	LastOrderAt  *time.Time `json:"lastOrderAt"`
	FirstOrderAt *time.Time `json:"firstOrderAt"`
	// LastShippingAddress is the destination from the buyer's latest cart
	// with shipping data. Nil when no cart has shipping yet.
	LastShippingAddress *CustomerShippingAddressResponse `json:"lastShippingAddress,omitempty"`
}

// NewCustomerResponse maps a domain Customer to its API response. This is the
// controller's outbound mapper: presentation knows the domain; the domain never
// knows the response.
func NewCustomerResponse(c *domain.Customer) CustomerResponse {
	resp := CustomerResponse{
		ID:           c.ID().String(),
		Handle:       c.Handle(),
		Email:        c.Email(),
		Phone:        c.Phone(),
		Name:         c.Name(),
		Document:     c.Document(),
		TotalOrders:  c.TotalOrders(),
		TotalSpent:   c.TotalSpent(),
		LastOrderAt:  c.LastOrderAt(),
		FirstOrderAt: c.FirstOrderAt(),
	}
	if addr := c.LastShippingAddress(); addr != nil {
		resp.LastShippingAddress = &CustomerShippingAddressResponse{
			ZipCode:      addr.ZipCode(),
			Street:       addr.Street(),
			Number:       addr.Number(),
			Complement:   addr.Complement(),
			Neighborhood: addr.Neighborhood(),
			City:         addr.City(),
			State:        addr.State(),
		}
	}
	return resp
}

type ListCustomersResponse struct {
	Data       []CustomerResponse       `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// NewListCustomersResponse maps a slice of Customer entities to the list
// response, attaching the pagination envelope.
func NewListCustomersResponse(customers []*domain.Customer, pagination query.Pagination, total int) ListCustomersResponse {
	data := make([]CustomerResponse, len(customers))
	for i, c := range customers {
		data[i] = NewCustomerResponse(c)
	}
	return ListCustomersResponse{
		Data:       data,
		Pagination: query.NewPaginationResponse(pagination, total),
	}
}

type CustomerStatsResponse struct {
	TotalCustomers      int   `json:"totalCustomers"`
	ActiveCustomers     int   `json:"activeCustomers"`
	AvgSpentPerCustomer int64 `json:"avgSpentPerCustomer"`
}

// NewCustomerStatsResponse maps the aggregated stats entity to its response.
func NewCustomerStatsResponse(s *domain.CustomerStats) CustomerStatsResponse {
	return CustomerStatsResponse{
		TotalCustomers:      s.TotalCustomers(),
		ActiveCustomers:     s.ActiveCustomers(),
		AvgSpentPerCustomer: s.AvgSpentPerCustomer(),
	}
}

// CustomerOrderResponse is a flattened summary of a cart attached to a
// customer, optimized for the customer-detail drawer.
type CustomerOrderResponse struct {
	ID            string     `json:"id"`
	ShortID       int32      `json:"shortId"`
	Status        string     `json:"status"`
	PaymentStatus *string    `json:"paymentStatus"`
	TotalItems    int        `json:"totalItems"`
	TotalValue    int64      `json:"totalValue"`
	PaidAt        *time.Time `json:"paidAt"`
	CreatedAt     *time.Time `json:"createdAt"`
}

// NewListCustomerOrdersResponse maps a slice of order-summary entities.
func NewListCustomerOrdersResponse(orders []*domain.OrderSummary) []CustomerOrderResponse {
	out := make([]CustomerOrderResponse, len(orders))
	for i, o := range orders {
		out[i] = CustomerOrderResponse{
			ID:            o.ID(),
			ShortID:       o.ShortID(),
			Status:        o.Status(),
			PaymentStatus: o.PaymentStatus(),
			TotalItems:    o.TotalItems(),
			TotalValue:    o.TotalValue(),
			PaidAt:        o.PaidAt(),
			CreatedAt:     o.CreatedAt(),
		}
	}
	return out
}

// ============================================
// Service layer - Input types
// ============================================

type ListCustomersInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CustomerFilters
}

// UpsertCustomerInput is used to create or update a customer
type UpsertCustomerInput struct {
	StoreID        uuid.UUID
	PlatformUserID string
	PlatformHandle string
	Email          *string
	Phone          *string
}

// UpdateCustomerInput is used to update customer fields
type UpdateCustomerInput struct {
	Handle *string
	Email  *string
	Phone  *string
}

// ============================================
// Repository layer types
// ============================================

type ListCustomersParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    CustomerFilters
}

// CustomerShippingAddress is the parsed shape of carts.shipping_address.
// JSON tags mirror the camelCase keys the checkout flow writes via
// CheckoutShippingAddressInfo so json.Unmarshal in the repository hits the
// right fields.
type CustomerShippingAddress struct {
	ZipCode      string `json:"zipCode,omitempty"`
	Street       string `json:"street,omitempty"`
	Number       string `json:"number,omitempty"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
}

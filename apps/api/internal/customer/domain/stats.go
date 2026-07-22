package domain

import "time"

// CustomerStats is the aggregate of store-wide customer metrics returned by the
// stats endpoint.
type CustomerStats struct {
	totalCustomers      int
	activeCustomers     int
	avgSpentPerCustomer int64
}

// ReconstructStats rebuilds a CustomerStats from persistence.
func ReconstructStats(totalCustomers, activeCustomers int, avgSpentPerCustomer int64) *CustomerStats {
	return &CustomerStats{
		totalCustomers:      totalCustomers,
		activeCustomers:     activeCustomers,
		avgSpentPerCustomer: avgSpentPerCustomer,
	}
}

func (s *CustomerStats) TotalCustomers() int        { return s.totalCustomers }
func (s *CustomerStats) ActiveCustomers() int       { return s.activeCustomers }
func (s *CustomerStats) AvgSpentPerCustomer() int64 { return s.avgSpentPerCustomer }

// OrderSummary is a flattened cart summary attached to a customer, used by the
// customer-detail drawer.
type OrderSummary struct {
	id            string
	shortID       int32
	status        string
	paymentStatus *string
	totalItems    int
	totalValue    int64
	paidAt        *time.Time
	createdAt     *time.Time
}

// ReconstructOrderSummary rebuilds an OrderSummary from persistence.
func ReconstructOrderSummary(
	id string,
	shortID int32,
	status string,
	paymentStatus *string,
	totalItems int,
	totalValue int64,
	paidAt *time.Time,
	createdAt *time.Time,
) *OrderSummary {
	return &OrderSummary{
		id:            id,
		shortID:       shortID,
		status:        status,
		paymentStatus: paymentStatus,
		totalItems:    totalItems,
		totalValue:    totalValue,
		paidAt:        paidAt,
		createdAt:     createdAt,
	}
}

func (o *OrderSummary) ID() string             { return o.id }
func (o *OrderSummary) ShortID() int32         { return o.shortID }
func (o *OrderSummary) Status() string         { return o.status }
func (o *OrderSummary) PaymentStatus() *string { return o.paymentStatus }
func (o *OrderSummary) TotalItems() int        { return o.totalItems }
func (o *OrderSummary) TotalValue() int64      { return o.totalValue }
func (o *OrderSummary) PaidAt() *time.Time     { return o.paidAt }
func (o *OrderSummary) CreatedAt() *time.Time  { return o.createdAt }

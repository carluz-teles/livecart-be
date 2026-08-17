package domain

// Logistics holds the shipping snapshot and fulfillment state for an order (1:1 with orders).
type Logistics struct {
	orderID              string
	shippingServiceName  *string
	shippingCarrier      *string
	shippingCostCents    *int64
	shippingDeadlineDays *int32
	trackingToken        *string
	shipmentStatus       ShipmentStatus
	erpOrderState        string
}

func (l *Logistics) OrderID() string                { return l.orderID }
func (l *Logistics) ShipmentStatus() ShipmentStatus { return l.shipmentStatus }
func (l *Logistics) TrackingToken() *string         { return l.trackingToken }
func (l *Logistics) ERPOrderState() string          { return l.erpOrderState }

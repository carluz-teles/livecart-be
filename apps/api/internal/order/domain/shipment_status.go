package domain

import "errors"

// ShipmentStatus rastreia o estado de entrega em order_logistics.
type ShipmentStatus struct{ value string }

var (
	ShipmentPending   = ShipmentStatus{value: "pending"}
	ShipmentInTransit = ShipmentStatus{value: "in_transit"}
	ShipmentDelivered = ShipmentStatus{value: "delivered"}
	ShipmentIssue     = ShipmentStatus{value: "issue"}
)

var ErrInvalidShipmentStatus = errors.New("invalid shipment status")

func (s ShipmentStatus) String() string { return s.value }
func (s ShipmentStatus) IsZero() bool   { return s.value == "" }

package shipping

import "livecart/apps/api/internal/integration/providers"

// Re-export types from providers package for convenience.
type (
	Credentials    = providers.Credentials
	QuoteRequest   = providers.QuoteRequest
	QuoteOption    = providers.QuoteOption
	ShippingItem   = providers.ShippingItem
	ShippingZip    = providers.ShippingZip
	CarrierService = providers.CarrierService

	ShippingAddressPoint    = providers.ShippingAddressPoint
	CreateShipmentRequest   = providers.CreateShipmentRequest
	CreateShipmentResult    = providers.CreateShipmentResult
	AttachInvoiceRequest    = providers.AttachInvoiceRequest
	UploadInvoiceXMLRequest = providers.UploadInvoiceXMLRequest
	GenerateLabelsRequest   = providers.GenerateLabelsRequest
	GenerateLabelsResult    = providers.GenerateLabelsResult
	LabelTicket             = providers.LabelTicket
	TrackShipmentRequest    = providers.TrackShipmentRequest
	TrackShipmentResult     = providers.TrackShipmentResult
	TrackingEvent           = providers.TrackingEvent
	TrackingStatus          = providers.TrackingStatus
)

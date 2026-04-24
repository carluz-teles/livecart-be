package integration

import (
	"fmt"
	"io"
	"time"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// SHIPPING ADMIN HANDLERS — connect + order lifecycle
// =============================================================================
//
// Shipping providers do not share a single auth model: Melhor Envio uses
// OAuth (handled by the existing /oauth flow), SmartEnvios uses a static
// token. This file holds the SmartEnvios-specific connect endpoint plus
// provider-agnostic order-lifecycle endpoints that the admin UI calls after
// a pedido is paid (create shipment, attach NFe, generate labels, pull
// tracking). Routes are registered in handler.go:RegisterRoutes.

// =============================================================================
// CONNECT SMARTENVIOS
// =============================================================================

// ConnectSmartEnviosRequest is the body for POST /integrations/shipping/smartenvios/connect.
type ConnectSmartEnviosRequest struct {
	Token string `json:"token" validate:"required,min=10"`
	Env   string `json:"env,omitempty"` // "sandbox" | "production" — defaults to production
}

// ConnectSmartEnvios validates the supplied token against SmartEnvios and
// persists the integration as active. If the store already has a SmartEnvios
// integration, the token is rotated in place.
//
// @Summary Connect SmartEnvios
// @Description Validates the SmartEnvios token and activates the shipping integration for the store
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body ConnectSmartEnviosRequest true "SmartEnvios connection payload"
// @Success 200 {object} httpx.Envelope{data=IntegrationResponse}
// @Failure 400 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/shipping/smartenvios/connect [post]
// @Security BearerAuth
func (h *Handler) ConnectSmartEnvios(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req ConnectSmartEnviosRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.ConnectSmartEnvios(c.Context(), ConnectSmartEnviosInput{
		StoreID: storeID,
		Token:   req.Token,
		Env:     req.Env,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, toIntegrationResponse(output))
}

// =============================================================================
// LIST CARRIERS
// =============================================================================

// ShippingCarrierResponse is a single enabled carrier/service returned by the provider.
type ShippingCarrierResponse struct {
	ServiceID         string `json:"serviceId"`
	Service           string `json:"service"`
	Carrier           string `json:"carrier"`
	CarrierLogoURL    string `json:"carrierLogoUrl,omitempty"`
	InsuranceMaxCents int64  `json:"insuranceMaxCents,omitempty"`
}

// ListShippingCarriers lists the services enabled for the store's active
// shipping integration of the given provider.
//
// @Router /api/v1/stores/{storeId}/integrations/shipping/{provider}/carriers [get]
// @Security BearerAuth
func (h *Handler) ListShippingCarriers(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	providerName := providers.ProviderName(c.Params("provider"))

	sp, err := h.service.GetShippingProviderByName(c.Context(), storeID, providerName)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	carriers, err := sp.ListCarriers(c.Context())
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, toCarrierResponse(carriers))
}

// valueOrDefault returns v when non-empty, otherwise fallback.
func valueOrDefault(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func toCarrierResponse(in []providers.CarrierService) []ShippingCarrierResponse {
	out := make([]ShippingCarrierResponse, 0, len(in))
	for _, c := range in {
		out = append(out, ShippingCarrierResponse{
			ServiceID:         c.ServiceID,
			Service:           c.Service,
			Carrier:           c.Carrier,
			CarrierLogoURL:    c.CarrierLogo,
			InsuranceMaxCents: c.InsuranceMaxCents,
		})
	}
	return out
}

// =============================================================================
// CREATE SHIPMENT
// =============================================================================

// AddressRequest mirrors ShippingAddressPoint in a JSON-friendly way.
type AddressRequest struct {
	Name         string `json:"name,omitempty"`
	Document     string `json:"document,omitempty"`
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street,omitempty"`
	Number       string `json:"number,omitempty"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	Phone        string `json:"phone,omitempty"`
	Email        string `json:"email,omitempty"`
	Observation  string `json:"observation,omitempty"`
}

func (a AddressRequest) toDomain() providers.ShippingAddressPoint {
	return providers.ShippingAddressPoint{
		Name:         a.Name,
		Document:     a.Document,
		ZipCode:      a.ZipCode,
		Street:       a.Street,
		Number:       a.Number,
		Complement:   a.Complement,
		Neighborhood: a.Neighborhood,
		City:         a.City,
		State:        a.State,
		Phone:        a.Phone,
		Email:        a.Email,
		Observation:  a.Observation,
	}
}

// ShipmentItemRequest describes one item/volume being shipped.
type ShipmentItemRequest struct {
	ID                  string `json:"id,omitempty"`
	Name                string `json:"name,omitempty"`
	Quantity            int    `json:"quantity" validate:"required,gt=0"`
	UnitPriceCents      int64  `json:"unitPriceCents"`
	WeightGrams         int    `json:"weightGrams" validate:"required,gt=0"`
	HeightCm            int    `json:"heightCm" validate:"required,gt=0"`
	WidthCm             int    `json:"widthCm" validate:"required,gt=0"`
	LengthCm            int    `json:"lengthCm" validate:"required,gt=0"`
	InsuranceValueCents int64  `json:"insuranceValueCents,omitempty"`
	PackageFormat       string `json:"packageFormat,omitempty"`
}

// CreateShippingShipmentRequest is the body for POST /shipping/:provider/shipments.
type CreateShippingShipmentRequest struct {
	QuoteServiceID  string                `json:"quoteServiceId" validate:"required"`
	ExternalOrderID string                `json:"externalOrderId" validate:"required"`
	InvoiceKey      string                `json:"invoiceKey,omitempty"`
	Sender          AddressRequest        `json:"sender"`
	Destiny         AddressRequest        `json:"destiny" validate:"required"`
	Items           []ShipmentItemRequest `json:"items" validate:"required,min=1,dive"`
	VolumeCount     int                   `json:"volumeCount"`
	Observation     string                `json:"observation,omitempty"`
}

// CreateShippingShipmentResponse mirrors providers.CreateShipmentResult for the admin UI.
type CreateShippingShipmentResponse struct {
	ShipmentID          string    `json:"shipmentId"` // LiveCart-side shipment id (shipments.id)
	ProviderOrderID     string    `json:"providerOrderId"`
	ProviderOrderNumber string    `json:"providerOrderNumber,omitempty"`
	TrackingCode        string    `json:"trackingCode,omitempty"`
	InvoiceID           string    `json:"invoiceId,omitempty"`
	Status              string    `json:"status"`
	StatusRawCode       int       `json:"statusRawCode,omitempty"`
	StatusRawName       string    `json:"statusRawName,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
}

// CreateShippingShipment creates a new shipment at the chosen provider.
//
// @Router /api/v1/stores/{storeId}/integrations/shipping/{provider}/shipments [post]
// @Security BearerAuth
func (h *Handler) CreateShippingShipment(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	providerName := providers.ProviderName(c.Params("provider"))

	var req CreateShippingShipmentRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	osp, err := h.service.GetShippingOrderProvider(c.Context(), storeID, providerName)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	items := make([]providers.ShippingItem, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, providers.ShippingItem{
			ID:                  it.ID,
			Name:                it.Name,
			Quantity:            it.Quantity,
			UnitPriceCents:      it.UnitPriceCents,
			WeightGrams:         it.WeightGrams,
			HeightCm:            it.HeightCm,
			WidthCm:             it.WidthCm,
			LengthCm:            it.LengthCm,
			InsuranceValueCents: it.InsuranceValueCents,
			PackageFormat:       it.PackageFormat,
		})
	}
	out, err := osp.CreateShipment(c.Context(), providers.CreateShipmentRequest{
		QuoteServiceID:  req.QuoteServiceID,
		ExternalOrderID: req.ExternalOrderID,
		InvoiceKey:      req.InvoiceKey,
		Sender:          req.Sender.toDomain(),
		Destiny:         req.Destiny.toDomain(),
		Items:           items,
		VolumeCount:     req.VolumeCount,
		Observation:     req.Observation,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Persist so the admin UI can resume the flow on future page loads.
	// `externalOrderId` on the request maps to our internal cart/order id.
	invoiceKind := ""
	if req.InvoiceKey != "" {
		invoiceKind = "nfe"
	}
	shipment, err := h.service.repo.CreateShipment(c.Context(), CreateShipmentParams{
		OrderID:             req.ExternalOrderID,
		StoreID:             storeID,
		Provider:            string(providerName),
		ProviderOrderID:     out.ProviderOrderID,
		ProviderOrderNumber: out.ProviderOrderNumber,
		TrackingCode:        out.TrackingCode,
		InvoiceKey:          req.InvoiceKey,
		InvoiceKind:         invoiceKind,
		InvoiceID:           out.InvoiceID,
		Status:              string(out.Status),
		StatusRawCode:       out.StatusRawCode,
		StatusRawName:       out.StatusRawName,
		ProviderMeta:        out.ProviderMeta,
	})
	if err != nil {
		// Persistence failure is surfaced but the shipment DOES exist at the
		// provider — caller must retry with the same external_order_id to
		// reconcile (the ON CONFLICT handles the idempotency path).
		return httpx.HandleServiceError(c, fmt.Errorf("shipment created at provider but failed to persist locally: %w", err))
	}
	return httpx.Created(c, CreateShippingShipmentResponse{
		ShipmentID:          shipment.ID,
		ProviderOrderID:     out.ProviderOrderID,
		ProviderOrderNumber: out.ProviderOrderNumber,
		TrackingCode:        out.TrackingCode,
		InvoiceID:           out.InvoiceID,
		Status:              string(out.Status),
		StatusRawCode:       out.StatusRawCode,
		StatusRawName:       out.StatusRawName,
		CreatedAt:           out.CreatedAt,
	})
}

// =============================================================================
// ATTACH / UPLOAD INVOICE
// =============================================================================

// AttachShippingInvoiceRequest links an already-emitted NFe/DCe to a shipment.
type AttachShippingInvoiceRequest struct {
	ExternalOrderID string `json:"externalOrderId,omitempty"`
	InvoiceKey      string `json:"invoiceKey" validate:"required"`
	InvoiceKind     string `json:"invoiceKind,omitempty"` // "nfe" (default) | "dce"
}

// AttachShippingInvoice handles POST /shipping/:provider/shipments/:shipmentId/invoice.
//
// @Router /api/v1/stores/{storeId}/integrations/shipping/{provider}/shipments/{shipmentId}/invoice [post]
// @Security BearerAuth
func (h *Handler) AttachShippingInvoice(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	providerName := providers.ProviderName(c.Params("provider"))
	shipmentID := c.Params("shipmentId")

	var req AttachShippingInvoiceRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}
	kind := req.InvoiceKind
	if kind == "" {
		kind = "nfe"
	}

	osp, err := h.service.GetShippingOrderProvider(c.Context(), storeID, providerName)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// `shipmentId` on the path is the LiveCart-side id (shipments.id) — resolve
	// to the provider-side id before calling the provider.
	shipment, err := h.service.repo.getShipmentByIDForStore(c.Context(), shipmentID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	if err := osp.AttachInvoice(c.Context(), providers.AttachInvoiceRequest{
		ProviderOrderID: shipment.ProviderOrderID,
		ExternalOrderID: valueOrDefault(req.ExternalOrderID, shipment.OrderID),
		InvoiceKey:      req.InvoiceKey,
		InvoiceKind:     kind,
	}); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if err := h.service.repo.UpdateShipmentInvoice(c.Context(), shipment.ID, req.InvoiceKey, kind); err != nil {
		return httpx.HandleServiceError(c, fmt.Errorf("invoice attached at provider but failed to persist locally: %w", err))
	}
	return httpx.OK(c, map[string]string{"status": "invoice_attached"})
}

// UploadShippingInvoiceXML handles POST /shipping/:provider/shipments/:shipmentId/invoice-xml.
// Expects multipart/form-data with a single `file` field containing the NFe XML.
//
// @Router /api/v1/stores/{storeId}/integrations/shipping/{provider}/shipments/{shipmentId}/invoice-xml [post]
// @Security BearerAuth
func (h *Handler) UploadShippingInvoiceXML(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	providerName := providers.ProviderName(c.Params("provider"))
	shipmentID := c.Params("shipmentId")

	fh, err := c.FormFile("file")
	if err != nil {
		return httpx.BadRequest(c, "file is required (multipart/form-data field 'file')")
	}
	f, err := fh.Open()
	if err != nil {
		return httpx.BadRequest(c, "cannot read uploaded file: "+err.Error())
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return httpx.BadRequest(c, "cannot read uploaded file: "+err.Error())
	}

	osp, err := h.service.GetShippingOrderProvider(c.Context(), storeID, providerName)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	shipment, err := h.service.repo.getShipmentByIDForStore(c.Context(), shipmentID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if err := osp.UploadInvoiceXML(c.Context(), providers.UploadInvoiceXMLRequest{
		ProviderOrderID: shipment.ProviderOrderID,
		XML:             raw,
		Filename:        fh.Filename,
	}); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	// Upload flows do not return an invoice key; mark the shipment's invoice_kind
	// as 'nfe' so the admin UI knows XML was delivered for this shipment even
	// if the key is not yet known.
	if err := h.service.repo.UpdateShipmentInvoice(c.Context(), shipment.ID, shipment.InvoiceKey, "nfe"); err != nil {
		return httpx.HandleServiceError(c, fmt.Errorf("xml uploaded at provider but failed to persist locally: %w", err))
	}
	return httpx.OK(c, map[string]string{"status": "invoice_xml_uploaded"})
}

// =============================================================================
// GENERATE LABELS
// =============================================================================

// GenerateShippingLabelsRequest is the body for POST /shipping/:provider/labels.
// Exactly one of the identifier arrays should be populated.
type GenerateShippingLabelsRequest struct {
	ProviderOrderIDs []string `json:"providerOrderIds,omitempty"`
	TrackingCodes    []string `json:"trackingCodes,omitempty"`
	InvoiceKeys      []string `json:"invoiceKeys,omitempty"`
	ExternalOrderIDs []string `json:"externalOrderIds,omitempty"`
	Format           string   `json:"format,omitempty"`
	DocumentType     string   `json:"documentType,omitempty"`
}

// GenerateShippingLabelsResponse mirrors providers.GenerateLabelsResult.
type GenerateShippingLabelsResponse struct {
	LabelURL string                       `json:"labelUrl,omitempty"`
	Tickets  []GenerateShippingLabelEntry `json:"tickets"`
}

// GenerateShippingLabelEntry is a single ticket in the response.
type GenerateShippingLabelEntry struct {
	ProviderOrderID string   `json:"providerOrderId"`
	TrackingCode    string   `json:"trackingCode,omitempty"`
	PublicTracking  string   `json:"publicTracking,omitempty"`
	VolumeBarcodes  []string `json:"volumeBarcodes"`
}

// GenerateShippingLabels handles POST /shipping/:provider/labels.
//
// @Router /api/v1/stores/{storeId}/integrations/shipping/{provider}/labels [post]
// @Security BearerAuth
func (h *Handler) GenerateShippingLabels(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	providerName := providers.ProviderName(c.Params("provider"))

	var req GenerateShippingLabelsRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	osp, err := h.service.GetShippingOrderProvider(c.Context(), storeID, providerName)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	result, err := osp.GenerateLabels(c.Context(), providers.GenerateLabelsRequest{
		ProviderOrderIDs: req.ProviderOrderIDs,
		TrackingCodes:    req.TrackingCodes,
		InvoiceKeys:      req.InvoiceKeys,
		ExternalOrderIDs: req.ExternalOrderIDs,
		Format:           req.Format,
		DocumentType:     req.DocumentType,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Persist per-ticket data back to our shipments. Lookup by provider_order_id
	// (the one the carrier owns) so label URL + public tracking + tracking code
	// land on the right row. We ignore lookup misses — the admin may have
	// requested labels for a shipment that was never persisted locally.
	for _, t := range result.Tickets {
		if t.ProviderOrderID == "" {
			continue
		}
		sh, lookupErr := h.service.repo.GetShipmentByProviderOrderID(c.Context(), string(providerName), t.ProviderOrderID)
		if lookupErr != nil || sh == nil {
			continue
		}
		_ = h.service.repo.UpdateShipmentLabels(c.Context(), sh.ID, result.LabelURL, t.PublicTracking, t.TrackingCode)
	}

	tickets := make([]GenerateShippingLabelEntry, 0, len(result.Tickets))
	for _, t := range result.Tickets {
		tickets = append(tickets, GenerateShippingLabelEntry{
			ProviderOrderID: t.ProviderOrderID,
			TrackingCode:    t.TrackingCode,
			PublicTracking:  t.PublicTracking,
			VolumeBarcodes:  t.VolumeBarcodes,
		})
	}
	return httpx.OK(c, GenerateShippingLabelsResponse{
		LabelURL: result.LabelURL,
		Tickets:  tickets,
	})
}

// =============================================================================
// TRACKING (pull)
// =============================================================================

// TrackShippingRequest identifies which shipment to pull history for.
// Exactly one field should be populated.
type TrackShippingRequest struct {
	ProviderOrderID string `json:"providerOrderId,omitempty"`
	ExternalOrderID string `json:"externalOrderId,omitempty"`
	InvoiceKey      string `json:"invoiceKey,omitempty"`
	TrackingCode    string `json:"trackingCode,omitempty"`
}

// TrackShippingResponse mirrors providers.TrackShipmentResult.
type TrackShippingResponse struct {
	TrackingCode  string                 `json:"trackingCode"`
	Carrier       string                 `json:"carrier"`
	Service       string                 `json:"service"`
	CurrentStatus string                 `json:"currentStatus"`
	Events        []TrackShippingEvent   `json:"events"`
}

// TrackShippingEvent is a single timeline entry.
type TrackShippingEvent struct {
	Status      string    `json:"status"`
	RawCode     int       `json:"rawCode,omitempty"`
	RawName     string    `json:"rawName,omitempty"`
	Observation string    `json:"observation,omitempty"`
	EventAt     time.Time `json:"eventAt"`
}

// TrackShipping handles POST /shipping/:provider/tracking.
//
// @Router /api/v1/stores/{storeId}/integrations/shipping/{provider}/tracking [post]
// @Security BearerAuth
func (h *Handler) TrackShipping(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	providerName := providers.ProviderName(c.Params("provider"))

	var req TrackShippingRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	osp, err := h.service.GetShippingOrderProvider(c.Context(), storeID, providerName)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	result, err := osp.TrackShipment(c.Context(), providers.TrackShipmentRequest{
		ProviderOrderID: req.ProviderOrderID,
		ExternalOrderID: req.ExternalOrderID,
		InvoiceKey:      req.InvoiceKey,
		TrackingCode:    req.TrackingCode,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Persist events + update shipment current status. Best-effort: if we cannot
	// find a matching shipment locally (e.g. caller tracked by a tracking_code
	// that is not ours), we still return the data to the admin UI.
	var targetShipment *ShipmentRow
	if req.ProviderOrderID != "" {
		targetShipment, _ = h.service.repo.GetShipmentByProviderOrderID(c.Context(), string(providerName), req.ProviderOrderID)
	}
	if targetShipment == nil && result.TrackingCode != "" {
		targetShipment, _ = h.service.repo.getShipmentByTrackingCode(c.Context(), string(providerName), result.TrackingCode)
	}
	if targetShipment != nil {
		inputs := make([]TrackingEventInput, 0, len(result.Events))
		for _, e := range result.Events {
			inputs = append(inputs, TrackingEventInput{
				Status:      string(e.Status),
				RawCode:     e.RawCode,
				RawName:     e.RawName,
				Observation: e.Observation,
				EventAt:     e.EventAt,
				Source:      "poll",
			})
		}
		_ = h.service.repo.InsertTrackingEvents(c.Context(), targetShipment.ID, inputs)
		if len(result.Events) > 0 {
			last := result.Events[len(result.Events)-1]
			_ = h.service.repo.UpdateShipmentStatus(c.Context(), targetShipment.ID, string(result.CurrentStatus), last.RawCode, last.RawName, result.TrackingCode)
		} else if string(result.CurrentStatus) != "" {
			_ = h.service.repo.UpdateShipmentStatus(c.Context(), targetShipment.ID, string(result.CurrentStatus), 0, "", result.TrackingCode)
		}
	}

	events := make([]TrackShippingEvent, 0, len(result.Events))
	for _, e := range result.Events {
		events = append(events, TrackShippingEvent{
			Status:      string(e.Status),
			RawCode:     e.RawCode,
			RawName:     e.RawName,
			Observation: e.Observation,
			EventAt:     e.EventAt,
		})
	}
	return httpx.OK(c, TrackShippingResponse{
		TrackingCode:  result.TrackingCode,
		Carrier:       result.Carrier,
		Service:       result.Service,
		CurrentStatus: string(result.CurrentStatus),
		Events:        events,
	})
}

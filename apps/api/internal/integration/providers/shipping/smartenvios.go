package shipping

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// =============================================================================
// SMARTENVIOS PROVIDER
// =============================================================================
//
// Docs: https://dev.smartenvios.com/docs/smartenvios/
// Auth: static `token` header (embarcador/base/hub/transportadora token).
// Base URL: prod  `https://api.smartenvios.com/v1`
//           sbx   `https://sandbox.api.smartenvios.com` (no /v1 suffix)
//
// Scope implemented: Quote, ListCarriers, TestConnection + order lifecycle
// (CreateShipment, AttachInvoice, UploadInvoiceXML, GenerateLabels, Track).
// Webhooks are deferred — tracking is pull-based only.
//
// Notes captured from the spec (see docs/integrations/smartenvios.md):
//  • service id in the Quote response is an opaque string — stored verbatim
//    and sent back as `?quote_service_id=...` on /dc-create.
//  • dc-create accepts `application/json` when `nfe_key` is used (XML upload
//    is handled by the separate /nfe-upload endpoint).
//  • unit_price/total_price are integer reais (not cents) — values are rounded
//    at the boundary; precise fiscal values live in the NFe anyway.
//  • items[] responses may come with JSON keys containing whitespace — the
//    parser normalizes keys via a case-insensitive TrimSpace layer.
//  • There is no official cancellation endpoint; CancelShipment is surfaced as
//    ErrOperationNotSupported.

const (
	seEnvSandbox    = "sandbox"
	seEnvProduction = "production"

	seProdBaseURL    = "https://api.smartenvios.com/v1"
	seSandboxBaseURL = "https://sandbox.api.smartenvios.com"

	seQuotePath     = "/quote/freight"
	seServicesPath  = "/quote/services"
	seDcCreatePath  = "/dc-create"
	seNfeUploadPath = "/nfe-upload"
	seOrderPath     = "/order"
	seLabelsPath    = "/labels"
	seTrackingPath  = "/freight-order/tracking"

	// NfeUploadBaseID is the fixed base_id parameter required by /nfe-upload.
	// The SmartEnvios docs list this as a literal constant; validate in sandbox
	// if uploads start failing.
	seNfeUploadBaseID = "a66cb425-a04c-460a-a0ac-b5ef61367e50"

	seDefaultUserAgent = "LiveCart/1.0 (+https://livecart.com.br)"

	// seSourceTag identifies LiveCart as the originating CMS in quotes/orders.
	// Reused for `source` and `external_origin` when the caller does not
	// override them with a more specific value (e.g. cart id).
	seSourceTag = "LIVECART"
)

// SmartEnvios implements ShippingProvider + ShippingOrderProvider.
type SmartEnvios struct {
	*providers.BaseProvider
	credentials *Credentials
	env         string
	userAgent   string
}

// NewSmartEnvios constructs a SmartEnvios provider. Exported as the factory
// constructor target.
func NewSmartEnvios(cfg providers.SmartEnviosConfig) (providers.ShippingProvider, error) {
	if cfg.Credentials == nil || cfg.Credentials.AccessToken == "" {
		return nil, fmt.Errorf("smartenvios token is required (stored in Credentials.AccessToken)")
	}
	env := cfg.Env
	if env == "" {
		env = seEnvProduction // safer default — sandbox has to be opted into
	}
	if env != seEnvSandbox && env != seEnvProduction {
		return nil, fmt.Errorf("invalid env %q: must be 'sandbox' or 'production'", env)
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = seDefaultUserAgent
	}
	return &SmartEnvios{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       30 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		credentials: cfg.Credentials,
		env:         env,
		userAgent:   ua,
	}, nil
}

// baseURL returns the base URL for the configured environment.
func (s *SmartEnvios) baseURL() string {
	if s.env == seEnvSandbox {
		return seSandboxBaseURL
	}
	return seProdBaseURL
}

// =============================================================================
// PROVIDER METADATA
// =============================================================================

func (s *SmartEnvios) Type() providers.ProviderType { return providers.ProviderTypeShipping }
func (s *SmartEnvios) Name() providers.ProviderName { return providers.ProviderSmartEnvios }

// ValidateCredentials probes a cheap read-only endpoint.
func (s *SmartEnvios) ValidateCredentials(ctx context.Context) error {
	if _, err := s.ListCarriers(ctx); err != nil {
		return fmt.Errorf("invalid smartenvios token: %w", err)
	}
	return nil
}

// RefreshToken is a no-op — SmartEnvios uses a static token.
func (s *SmartEnvios) RefreshToken(ctx context.Context) (*Credentials, error) {
	return nil, nil
}

// TestConnection performs a read-only probe via /quote/services.
func (s *SmartEnvios) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	carriers, err := s.ListCarriers(ctx)
	latency := time.Since(start)
	if err != nil {
		return &providers.TestConnectionResult{
			Success:  false,
			Message:  err.Error(),
			Latency:  latency,
			TestedAt: time.Now(),
		}, nil
	}
	return &providers.TestConnectionResult{
		Success:  true,
		Message:  fmt.Sprintf("connected to smartenvios (%s), %d services enabled", s.env, len(carriers)),
		Latency:  latency,
		TestedAt: time.Now(),
		AccountInfo: map[string]any{
			"env":           s.env,
			"service_count": len(carriers),
		},
	}, nil
}

// =============================================================================
// QUOTE
// =============================================================================

type seQuoteVolume struct {
	Weight   float64  `json:"weight"`
	Height   int      `json:"height"`
	Length   int      `json:"length"`
	Width    int      `json:"width"`
	Quantity int      `json:"quantity"`
	Price    float64  `json:"price"`
	SKU      []string `json:"sku,omitempty"`
}

type seQuoteRequest struct {
	TotalPrice     float64         `json:"total_price"`
	ZipCodeStart   string          `json:"zip_code_start"`
	ZipCodeEnd     string          `json:"zip_code_end"`
	Source         string          `json:"source,omitempty"`
	ExternalOrigin string          `json:"external_origin,omitempty"`
	Volumes        []seQuoteVolume `json:"volumes"`
}

type seQuoteOption struct {
	ID          string   `json:"id"`
	Base        string   `json:"base"`
	ServiceCode int      `json:"service_code"`
	Service     string   `json:"service"`
	Value       float64  `json:"value"`
	Days        int      `json:"days"`
	IsValid     bool     `json:"is_valid"`
	Errors      []string `json:"errors"`
}

type seQuoteResponse struct {
	Result []seQuoteOption `json:"result"`
}

// Quote calculates shipping costs.
func (s *SmartEnvios) Quote(ctx context.Context, req QuoteRequest) ([]QuoteOption, error) {
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("at least one item is required")
	}
	from := sanitizeCEPHyphen(string(req.FromZip))
	to := sanitizeCEPHyphen(string(req.ToZip))
	if from == "" || to == "" {
		return nil, fmt.Errorf("from_zip and to_zip are required")
	}

	volumes := make([]seQuoteVolume, 0, len(req.Items))
	var totalPrice float64
	heaviestIdx := 0
	var heaviestGrams int
	for i, it := range req.Items {
		if it.WeightGrams <= 0 || it.HeightCm <= 0 || it.WidthCm <= 0 || it.LengthCm <= 0 {
			return nil, fmt.Errorf("item %d: dimensions and weight must be positive", i)
		}
		if it.Quantity <= 0 {
			return nil, fmt.Errorf("item %d: quantity must be > 0", i)
		}
		if it.WeightGrams > heaviestGrams {
			heaviestGrams = it.WeightGrams
			heaviestIdx = i
		}
		unitPrice := float64(it.InsuranceValueCents) / 100.0
		if unitPrice <= 0 {
			unitPrice = float64(it.UnitPriceCents) / 100.0
		}
		totalPrice += unitPrice * float64(it.Quantity)
		vol := seQuoteVolume{
			Weight:   float64(it.WeightGrams) / 1000.0,
			Height:   it.HeightCm,
			Length:   it.LengthCm,
			Width:    it.WidthCm,
			Quantity: it.Quantity,
			Price:    unitPrice,
		}
		if it.ID != "" {
			vol.SKU = []string{it.ID}
		}
		volumes = append(volumes, vol)
	}
	if req.ExtraPackageWeightGrams > 0 && len(volumes) > 0 {
		volumes[heaviestIdx].Weight += float64(req.ExtraPackageWeightGrams) / 1000.0
	}

	body := seQuoteRequest{
		TotalPrice:     math.Round(totalPrice*100) / 100,
		ZipCodeStart:   from,
		ZipCodeEnd:     to,
		Source:         seSourceTag,
		ExternalOrigin: req.ExternalID,
		Volumes:        volumes,
	}

	respBody, err := s.doJSON(ctx, http.MethodPost, seQuotePath, body, nil)
	if err != nil {
		return nil, err
	}
	var parsed seQuoteResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing smartenvios quote response: %w, body=%s", err, string(respBody))
	}

	// Apply the caller-side ServiceIDs filter (SmartEnvios does not support
	// per-service quoting natively — it always returns the full list).
	allowed := map[string]bool{}
	for _, id := range req.ServiceIDs {
		allowed[id] = true
	}

	options := make([]QuoteOption, 0, len(parsed.Result))
	for _, r := range parsed.Result {
		if len(allowed) > 0 && !allowed[r.ID] {
			continue
		}
		opt := QuoteOption{
			Provider:     providers.ProviderSmartEnvios,
			ServiceID:    r.ID,
			Service:      r.Service,
			Carrier:      carrierFromServiceName(r.Service),
			PriceCents:   int64(math.Round(r.Value * 100)),
			DeadlineDays: r.Days,
			Available:    r.IsValid,
			Error:        strings.Join(r.Errors, "; "),
		}
		options = append(options, opt)
	}
	return options, nil
}

// ListCarriers returns the enabled services via /quote/services.
type seServiceListEntry struct {
	FreightSheetID          string `json:"freight_sheet_id"`
	FreightSheetDescription string `json:"freight_sheet_description"`
}

func (s *SmartEnvios) ListCarriers(ctx context.Context) ([]CarrierService, error) {
	respBody, err := s.doJSON(ctx, http.MethodGet, seServicesPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var entries []seServiceListEntry
	if err := json.Unmarshal(respBody, &entries); err != nil {
		return nil, fmt.Errorf("parsing smartenvios services response: %w, body=%s", err, string(respBody))
	}
	out := make([]CarrierService, 0, len(entries))
	for _, e := range entries {
		out = append(out, CarrierService{
			ServiceID: e.FreightSheetID,
			Service:   e.FreightSheetDescription,
			Carrier:   carrierFromServiceName(e.FreightSheetDescription),
		})
	}
	return out, nil
}

// =============================================================================
// CREATE SHIPMENT (dc-create)
// =============================================================================

type seDcCreateItem struct {
	Description string   `json:"description"`
	Amount      int      `json:"amount"`
	UnitPrice   int      `json:"unit_price"`   // reais inteiros
	TotalPrice  int      `json:"total_price"`  // reais inteiros
	Weight      float64  `json:"weight"`       // kg
	Height      int      `json:"height"`       // cm
	Width       int      `json:"width"`        // cm
	Length      int      `json:"length"`       // cm
	SKU         []string `json:"sku,omitempty"`
}

type seDcCreateAddress struct {
	SenderName         string `json:"sender_name,omitempty"`
	SenderDocument     string `json:"sender_document,omitempty"`
	SenderZipcode      string `json:"sender_zipcode,omitempty"`
	SenderStreet       string `json:"sender_street,omitempty"`
	SenderNumber       string `json:"sender_number,omitempty"`
	SenderNeighborhood string `json:"sender_neighborhood,omitempty"`
	SenderComplement   string `json:"sender_complement,omitempty"`
	SenderPhone        string `json:"sender_phone,omitempty"`
	SenderEmail        string `json:"sender_email,omitempty"`
	Observation        string `json:"observation,omitempty"`

	DestinyDocument     string `json:"destiny_document,omitempty"`
	DestinyName         string `json:"destiny_name,omitempty"`
	DestinyZipcode      string `json:"destiny_zipcode"`
	DestinyStreet       string `json:"destiny_street,omitempty"`
	DestinyPhone        string `json:"destiny_phone,omitempty"`
	DestinyNumber       string `json:"destiny_number,omitempty"`
	DestinyNeighborhood string `json:"destiny_neighborhood,omitempty"`
	DestinyComplement   string `json:"destiny_complement,omitempty"`
	DestinyEmail        string `json:"destiny_email,omitempty"`

	NfeKey                 string           `json:"nfe_key,omitempty"`
	DceKey                 string           `json:"dce_key,omitempty"`
	AdjustedVolumeQuantity int              `json:"adjusted_volume_quantity,omitempty"`
	Items                  []seDcCreateItem `json:"items"`
}

type seDcCreateRequest struct {
	PreferenceBy            string            `json:"preference_by,omitempty"` // unused in path A
	ExternalOrderID         string            `json:"external_order_id,omitempty"`
	ExternalOrigin          string            `json:"external_origin,omitempty"`
	FreightContentStatement seDcCreateAddress `json:"freightContentStatement"`
}

type seDcCreateResponse struct {
	Result struct {
		FreightOrderID           string `json:"freight_order_id"`
		FreightOrderNumber       int64  `json:"freight_order_number"`
		FreightOrderTrackingCode string `json:"freight_order_tracking_code"`
		CustomerID               string `json:"customer_id"`
		NfeID                    string `json:"nfe_id"`
		CreatedAt                string `json:"created_at"`
		UpdatedAt                string `json:"updated_at"`
		FreightOrderStatus       struct {
			Code int    `json:"code"`
			Name string `json:"name"`
		} `json:"freight_order_status"`
	} `json:"result"`
}

// CreateShipment opens a dc-create freight order.
func (s *SmartEnvios) CreateShipment(ctx context.Context, req CreateShipmentRequest) (*CreateShipmentResult, error) {
	if req.QuoteServiceID == "" {
		return nil, fmt.Errorf("quote_service_id is required")
	}
	if req.Destiny.ZipCode == "" {
		return nil, fmt.Errorf("destiny.zip_code is required")
	}
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("at least one item is required")
	}

	items := make([]seDcCreateItem, 0, len(req.Items))
	for _, it := range req.Items {
		qty := it.Quantity
		if qty <= 0 {
			qty = 1
		}
		unitPriceReais := int(math.Round(float64(it.UnitPriceCents) / 100.0))
		totalPriceReais := unitPriceReais * qty
		description := it.Name
		if description == "" {
			description = it.ID
		}
		if description == "" {
			description = "Produto"
		}
		seItem := seDcCreateItem{
			Description: description,
			Amount:      qty,
			UnitPrice:   unitPriceReais,
			TotalPrice:  totalPriceReais,
			Weight:      float64(it.WeightGrams) / 1000.0,
			Height:      it.HeightCm,
			Width:       it.WidthCm,
			Length:      it.LengthCm,
		}
		if it.ID != "" {
			seItem.SKU = []string{it.ID}
		}
		items = append(items, seItem)
	}

	volumeQty := req.VolumeCount
	if volumeQty <= 0 {
		volumeQty = 1
	}

	body := seDcCreateRequest{
		ExternalOrderID: req.ExternalOrderID,
		ExternalOrigin:  seSourceTag,
		FreightContentStatement: seDcCreateAddress{
			SenderName:         req.Sender.Name,
			SenderDocument:     req.Sender.Document,
			SenderZipcode:      sanitizeCEPHyphen(req.Sender.ZipCode),
			SenderStreet:       req.Sender.Street,
			SenderNumber:       req.Sender.Number,
			SenderNeighborhood: req.Sender.Neighborhood,
			SenderComplement:   req.Sender.Complement,
			SenderPhone:        req.Sender.Phone,
			SenderEmail:        req.Sender.Email,
			Observation:        valueOr(req.Observation, req.Sender.Observation),

			DestinyDocument:     req.Destiny.Document,
			DestinyName:         req.Destiny.Name,
			DestinyZipcode:      sanitizeCEPHyphen(req.Destiny.ZipCode),
			DestinyStreet:       req.Destiny.Street,
			DestinyPhone:        req.Destiny.Phone,
			DestinyNumber:       req.Destiny.Number,
			DestinyNeighborhood: req.Destiny.Neighborhood,
			DestinyComplement:   req.Destiny.Complement,
			DestinyEmail:        req.Destiny.Email,

			NfeKey:                 req.InvoiceKey,
			AdjustedVolumeQuantity: volumeQty,
			Items:                  items,
		},
	}

	path := seDcCreatePath + "?quote_service_id=" + req.QuoteServiceID
	respBody, err := s.doJSON(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return nil, err
	}
	var parsed seDcCreateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing smartenvios dc-create response: %w, body=%s", err, string(respBody))
	}

	var meta map[string]any
	_ = json.Unmarshal(respBody, &meta)
	createdAt, _ := parseSmartEnviosTime(parsed.Result.CreatedAt)

	return &CreateShipmentResult{
		ProviderOrderID:     parsed.Result.FreightOrderID,
		ProviderOrderNumber: fmt.Sprintf("%d", parsed.Result.FreightOrderNumber),
		TrackingCode:        parsed.Result.FreightOrderTrackingCode,
		InvoiceID:           parsed.Result.NfeID,
		Status:              mapSmartEnviosStatus(parsed.Result.FreightOrderStatus.Code),
		StatusRawCode:       parsed.Result.FreightOrderStatus.Code,
		StatusRawName:       parsed.Result.FreightOrderStatus.Name,
		CreatedAt:           createdAt,
		ProviderMeta:        meta,
	}, nil
}

// =============================================================================
// ATTACH INVOICE (PATCH /order with nfe_key)
// =============================================================================

func (s *SmartEnvios) AttachInvoice(ctx context.Context, req AttachInvoiceRequest) error {
	if req.InvoiceKey == "" {
		return fmt.Errorf("invoice_key is required")
	}
	if req.ProviderOrderID == "" && req.ExternalOrderID == "" {
		return fmt.Errorf("one of provider_order_id or external_order_id is required")
	}
	body := map[string]any{}
	if req.ExternalOrderID != "" {
		body["external_order_id"] = req.ExternalOrderID
	}
	if req.InvoiceKind == "dce" {
		// Spec does not show a dce_key on /order, but we keep the same body
		// shape as dc-create for forward-compat — SmartEnvios typically
		// ignores unknown fields.
		body["dce_key"] = req.InvoiceKey
	} else {
		body["nfe_key"] = req.InvoiceKey
	}

	// Identifier on the query string (the spec mentions "3 identificadores na URL"
	// but never declares a path-param — query is the safest assumption; if
	// SmartEnvios rejects this shape, fall back to /nfe-upload).
	path := seOrderPath
	if req.ProviderOrderID != "" {
		path += "?freight_order_id=" + req.ProviderOrderID
	}
	_, err := s.doJSON(ctx, http.MethodPatch, path, body, nil)
	return err
}

// =============================================================================
// UPLOAD INVOICE XML (nfe-upload, multipart)
// =============================================================================

func (s *SmartEnvios) UploadInvoiceXML(ctx context.Context, req UploadInvoiceXMLRequest) error {
	if req.ProviderOrderID == "" {
		return fmt.Errorf("provider_order_id is required")
	}
	if len(req.XML) == 0 {
		return fmt.Errorf("xml payload is required")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filename := req.Filename
	if filename == "" {
		filename = "nfe.xml"
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="form"; filename="%s"`, filename))
	header.Set("Content-Type", "application/xml")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating multipart part: %w", err)
	}
	if _, err := part.Write(req.XML); err != nil {
		return fmt.Errorf("writing xml: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}

	q := fmt.Sprintf("?base_id=%s&freight_order_id=%s", seNfeUploadBaseID, req.ProviderOrderID)
	url := s.baseURL() + seNfeUploadPath + q

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return fmt.Errorf("creating nfe-upload request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("token", s.credentials.AccessToken)
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	if s.userAgent != "" {
		httpReq.Header.Set("User-Agent", s.userAgent)
	}

	resp, err := s.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("executing nfe-upload: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("smartenvios nfe-upload failed: status %d, body: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// =============================================================================
// GENERATE LABELS
// =============================================================================

type seLabelsRequest struct {
	TrackingCodes []string `json:"tracking_codes,omitempty"`
	OrderIDs      []string `json:"order_ids,omitempty"`
	NfeKeys       []string `json:"nfe_keys,omitempty"`
	ExternalID    []string `json:"external_id,omitempty"`
	Type          string   `json:"type,omitempty"`
	DocumentType  string   `json:"documentType,omitempty"`
}

type seLabelTicket struct {
	FreightOrderID string `json:"freight_order_id"`
	TrackingCode   string `json:"tracking_code"`
	PublicTracking string `json:"public_tracking"`
	Volumes        []struct {
		Barcode string `json:"barcode"`
	} `json:"volumes"`
}

// seLabelsResponseEntry captures the inner shape of result[] in /labels.
// SmartEnvios returns an object with `url` and `tickets[]`; we keep it flat to
// surface both in the normalized result.
type seLabelsResponseEntry struct {
	URL     string          `json:"url"`
	Tickets []seLabelTicket `json:"tickets"`
}

type seLabelsResponse struct {
	Result []seLabelsResponseEntry `json:"result"`
}

func (s *SmartEnvios) GenerateLabels(ctx context.Context, req GenerateLabelsRequest) (*GenerateLabelsResult, error) {
	if len(req.ProviderOrderIDs) == 0 && len(req.TrackingCodes) == 0 && len(req.InvoiceKeys) == 0 && len(req.ExternalOrderIDs) == 0 {
		return nil, fmt.Errorf("one of provider_order_ids, tracking_codes, invoice_keys, external_order_ids is required")
	}
	body := seLabelsRequest{
		TrackingCodes: req.TrackingCodes,
		OrderIDs:      req.ProviderOrderIDs,
		NfeKeys:       req.InvoiceKeys,
		ExternalID:    req.ExternalOrderIDs,
		Type:          valueOr(req.Format, "pdf"),
		DocumentType:  req.DocumentType,
	}
	// Sync mode keeps things simple without webhooks.
	headers := map[string]string{"x-processing-mode": "sync"}
	respBody, err := s.doJSON(ctx, http.MethodPost, seLabelsPath, body, headers)
	if err != nil {
		return nil, err
	}

	// Try the documented array shape first; fall back to a single-object shape
	// (the spec sample is inconsistent with the schema).
	var parsed seLabelsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil || len(parsed.Result) == 0 {
		var single struct {
			Result seLabelsResponseEntry `json:"result"`
		}
		if err := json.Unmarshal(respBody, &single); err != nil {
			return nil, fmt.Errorf("parsing smartenvios labels response: %w, body=%s", err, string(respBody))
		}
		parsed.Result = []seLabelsResponseEntry{single.Result}
	}

	result := &GenerateLabelsResult{}
	for _, entry := range parsed.Result {
		if result.LabelURL == "" {
			result.LabelURL = entry.URL
		}
		for _, t := range entry.Tickets {
			barcodes := make([]string, 0, len(t.Volumes))
			for _, v := range t.Volumes {
				barcodes = append(barcodes, v.Barcode)
			}
			result.Tickets = append(result.Tickets, LabelTicket{
				ProviderOrderID: t.FreightOrderID,
				TrackingCode:    t.TrackingCode,
				PublicTracking:  t.PublicTracking,
				VolumeBarcodes:  barcodes,
			})
		}
	}
	return result, nil
}

// =============================================================================
// TRACKING (pull)
// =============================================================================

type seTrackingRequest struct {
	FreightOrderID string `json:"freight_order_id,omitempty"`
	OrderID        string `json:"order_id,omitempty"`
	NfeKey         string `json:"nfe_key,omitempty"`
	TrackingCode   string `json:"tracking_code,omitempty"`
}

type seTrackingEventCode struct {
	Number       int    `json:"number"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	TrackingType string `json:"tracking_type"`
}

type seTrackingEvent struct {
	Observation string              `json:"observation"`
	Date        string              `json:"date"`
	Code        seTrackingEventCode `json:"code"`
}

type seTrackingResponse struct {
	Result struct {
		Number        string            `json:"number"`
		TrackingCode  string            `json:"tracking_code"`
		ShippingName  string            `json:"shipping_name"`
		ServiceName   string            `json:"service_name"`
		Trackings     []seTrackingEvent `json:"trackings"`
	} `json:"result"`
}

func (s *SmartEnvios) TrackShipment(ctx context.Context, req TrackShipmentRequest) (*TrackShipmentResult, error) {
	body := seTrackingRequest{
		FreightOrderID: req.ProviderOrderID,
		OrderID:        req.ExternalOrderID,
		NfeKey:         req.InvoiceKey,
		TrackingCode:   req.TrackingCode,
	}
	set := 0
	if body.FreightOrderID != "" {
		set++
	}
	if body.OrderID != "" {
		set++
	}
	if body.NfeKey != "" {
		set++
	}
	if body.TrackingCode != "" {
		set++
	}
	if set != 1 {
		return nil, fmt.Errorf("exactly one of provider_order_id / external_order_id / invoice_key / tracking_code must be set")
	}

	respBody, err := s.doJSON(ctx, http.MethodPost, seTrackingPath, body, nil)
	if err != nil {
		return nil, err
	}
	var parsed seTrackingResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing smartenvios tracking response: %w, body=%s", err, string(respBody))
	}

	events := make([]TrackingEvent, 0, len(parsed.Result.Trackings))
	var last TrackingStatus
	for _, t := range parsed.Result.Trackings {
		at, _ := parseSmartEnviosTime(t.Date)
		status := mapSmartEnviosStatus(t.Code.Number)
		events = append(events, TrackingEvent{
			Status:      status,
			RawCode:     t.Code.Number,
			RawName:     t.Code.Name,
			Observation: t.Observation,
			EventAt:     at,
		})
		last = status
	}

	var meta map[string]any
	_ = json.Unmarshal(respBody, &meta)
	return &TrackShipmentResult{
		TrackingCode:  parsed.Result.TrackingCode,
		Carrier:       parsed.Result.ShippingName,
		Service:       parsed.Result.ServiceName,
		CurrentStatus: last,
		Events:        events,
		ProviderMeta:  meta,
	}, nil
}

// =============================================================================
// HTTP PLUMBING
// =============================================================================

// doJSON performs an authenticated JSON request to SmartEnvios. extraHeaders
// is optional (for `x-processing-mode`, etc.).
func (s *SmartEnvios) doJSON(ctx context.Context, method, path string, body any, extraHeaders map[string]string) ([]byte, error) {
	url := s.baseURL() + path

	var reqBody io.Reader
	if body != nil && method != http.MethodGet {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("token", s.credentials.AccessToken)
	if body != nil && method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.userAgent != "" {
		req.Header.Set("User-Agent", s.userAgent)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("smartenvios %s %s failed: status %d, body: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// =============================================================================
// HELPERS
// =============================================================================

// sanitizeCEPHyphen returns a CEP in the XXXXX-XXX format preferred by the
// SmartEnvios examples. Non-digit characters are dropped; shorter inputs are
// returned as-is (provider may still accept / will surface a clear error).
func sanitizeCEPHyphen(raw string) string {
	digits := make([]byte, 0, 8)
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits = append(digits, byte(r))
		}
	}
	if len(digits) != 8 {
		return string(digits)
	}
	return string(digits[:5]) + "-" + string(digits[5:])
}

// carrierFromServiceName extracts the carrier brand from a SmartEnvios
// service name (e.g. "Jadlog Package" → "Jadlog"). Returns the full string
// when it cannot be split.
func carrierFromServiceName(service string) string {
	service = strings.TrimSpace(service)
	if service == "" {
		return ""
	}
	if idx := strings.Index(service, " "); idx > 0 {
		return service[:idx]
	}
	return service
}

// parseSmartEnviosTime parses the two date shapes the API returns:
//   - "2022-01-06T20:13:27.523Z"
//   - "2022-11-14 11:38:31" (no timezone — assumed America/Sao_Paulo).
func parseSmartEnviosTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized smartenvios timestamp: %q", raw)
}

// mapSmartEnviosStatus translates SmartEnvios status codes to the LiveCart
// normalized TrackingStatus enum. See docs/integrations/smartenvios.md §14.
func mapSmartEnviosStatus(code int) TrackingStatus {
	switch code {
	case 0:
		return providers.TrackingStatusAwaitingInvoice
	case 1, 27, 32:
		return providers.TrackingStatusPending
	case 2:
		return providers.TrackingStatusPendingPickup
	case 28:
		return providers.TrackingStatusPendingDropoff
	case 3, 4, 5, 6, 16, 23:
		return providers.TrackingStatusInTransit
	case 25:
		return providers.TrackingStatusAwaitingPickup
	case 29:
		return providers.TrackingStatusOutForDelivery
	case 7:
		return providers.TrackingStatusDelivered
	case 8:
		return providers.TrackingStatusDeliveryIssue
	case 9:
		return providers.TrackingStatusDeliveryBlocked
	case 10, 24:
		return providers.TrackingStatusIssue
	case 11:
		return providers.TrackingStatusShipmentBlocked
	case 17, 22:
		return providers.TrackingStatusDamaged
	case 18:
		return providers.TrackingStatusStolen
	case 19:
		return providers.TrackingStatusLost
	case 20:
		return providers.TrackingStatusFiscalIssue
	case 21:
		return providers.TrackingStatusRefused
	case 26:
		return providers.TrackingStatusNotDelivered
	case 33:
		return providers.TrackingStatusIndemnificationRequested
	case 34:
		return providers.TrackingStatusIndemnificationScheduled
	case 35:
		return providers.TrackingStatusIndemnificationCompleted
	case 13, 30, 31:
		return providers.TrackingStatusReturning
	case 14, 15:
		return providers.TrackingStatusReturned
	case 12:
		return providers.TrackingStatusCanceled
	default:
		return providers.TrackingStatusUnknown
	}
}

// valueOr returns first when non-empty, otherwise second.
func valueOr(first, second string) string {
	if strings.TrimSpace(first) != "" {
		return first
	}
	return second
}

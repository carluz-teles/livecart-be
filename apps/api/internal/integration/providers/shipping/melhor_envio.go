package shipping

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

const (
	meEnvSandbox    = "sandbox"
	meEnvProduction = "production"

	meSandboxBaseURL = "https://sandbox.melhorenvio.com.br"
	meProdBaseURL    = "https://melhorenvio.com.br"

	meAuthPath  = "/oauth/authorize"
	meTokenPath = "/oauth/token"

	meCalculatePath = "/api/v2/me/shipment/calculate"
	meCompaniesPath = "/api/v2/me/shipment/companies"
	meProfilePath   = "/api/v2/me"
	meCartPath      = "/api/v2/me/cart"
	meCheckoutPath  = "/api/v2/me/shipment/checkout"
	meGeneratePath  = "/api/v2/me/shipment/generate"
	mePrintPath     = "/api/v2/me/shipment/print"
	meTrackingPath  = "/api/v2/me/shipment/tracking"
	meCancelPath    = "/api/v2/me/shipment/cancel"
)

// AccountInfo is the minimum subset of /api/v2/me needed by the admin UI.
type AccountInfo struct {
	Email string
	Name  string
}

// FetchAccountInfo retrieves the authenticated account email/name after an
// OAuth exchange. Best-effort: callers should treat errors as non-fatal and
// proceed without the extra metadata.
func FetchAccountInfo(ctx context.Context, env, accessToken, userAgent string) (AccountInfo, error) {
	base := meSandboxBaseURL
	if env == meEnvProduction {
		base = meProdBaseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+meProfilePath, nil)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("creating profile request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("executing profile request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return AccountInfo{}, fmt.Errorf("profile request failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Email     string `json:"email"`
		FirstName string `json:"firstname"`
		LastName  string `json:"lastname"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return AccountInfo{}, fmt.Errorf("parsing profile: %w", err)
	}

	name := strings.TrimSpace(parsed.Name)
	if name == "" {
		name = strings.TrimSpace(parsed.FirstName + " " + parsed.LastName)
	}
	return AccountInfo{Email: parsed.Email, Name: name}, nil
}

// MelhorEnvio implements the ShippingProvider interface for Melhor Envio.
// Only read endpoints are used: quote + list carriers. No labels are generated.
type MelhorEnvio struct {
	*providers.BaseProvider
	credentials  *Credentials
	clientID     string
	clientSecret string
	env          string
	userAgent    string
	redirectURI  string
}

// New creates a Melhor Envio provider. Exported as the factory constructor target.
func New(cfg providers.MelhorEnvioConfig) (providers.ShippingProvider, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("credentials are required")
	}
	if cfg.UserAgent == "" {
		return nil, fmt.Errorf("user_agent is required (format: 'AppName (contact@email.com)')")
	}
	env := cfg.Env
	if env == "" {
		env = meEnvSandbox
	}
	if env != meEnvSandbox && env != meEnvProduction {
		return nil, fmt.Errorf("invalid env %q: must be 'sandbox' or 'production'", env)
	}

	return &MelhorEnvio{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       30 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		credentials:  cfg.Credentials,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		env:          env,
		userAgent:    cfg.UserAgent,
		redirectURI:  cfg.RedirectURI,
	}, nil
}

// baseURL returns the base URL for the configured environment.
func (m *MelhorEnvio) baseURL() string {
	if m.env == meEnvProduction {
		return meProdBaseURL
	}
	return meSandboxBaseURL
}

// Type returns the provider type.
func (m *MelhorEnvio) Type() providers.ProviderType { return providers.ProviderTypeShipping }

// Name returns the provider name.
func (m *MelhorEnvio) Name() providers.ProviderName { return providers.ProviderMelhorEnvio }

// ValidateCredentials checks credentials by listing available carriers.
func (m *MelhorEnvio) ValidateCredentials(ctx context.Context) error {
	_, err := m.ListCarriers(ctx)
	if err != nil {
		return fmt.Errorf("invalid credentials: %w", err)
	}
	return nil
}

// TestConnection performs a simple read-only probe.
func (m *MelhorEnvio) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	carriers, err := m.ListCarriers(ctx)
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
		Message:  fmt.Sprintf("connected to melhor envio (%s), %d carriers", m.env, len(carriers)),
		Latency:  latency,
		TestedAt: time.Now(),
		AccountInfo: map[string]any{
			"env":           m.env,
			"carrier_count": len(carriers),
		},
	}, nil
}

// =============================================================================
// OAUTH: TOKEN REFRESH
// =============================================================================

// RefreshToken refreshes the OAuth access token using the refresh token.
// Melhor Envio tokens: access_token valid 30 days, refresh_token valid 45 days.
func (m *MelhorEnvio) RefreshToken(ctx context.Context) (*Credentials, error) {
	if m.credentials.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token available")
	}
	if m.clientID == "" || m.clientSecret == "" {
		return nil, fmt.Errorf("client_id or client_secret not configured")
	}

	body := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     m.clientID,
		"client_secret": m.clientSecret,
		"refresh_token": m.credentials.RefreshToken,
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling refresh body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL()+meTokenPath, strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("creating refresh request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", m.userAgent)

	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token failed: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp struct {
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing refresh response: %w", err)
	}

	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		expiresInSeconds = 30 * 24 * 3600 // default 30 days
	}

	m.Logger.Info("melhor envio token refresh successful",
		zap.Int("expires_in", expiresInSeconds),
		zap.Bool("has_new_refresh_token", tokenResp.RefreshToken != ""),
	)

	refreshed := &Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(expiresInSeconds) * time.Second),
		Extra:        m.credentials.Extra,
	}
	// If a new refresh_token was not returned, keep the previous one.
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = m.credentials.RefreshToken
	}
	m.credentials = refreshed
	return refreshed, nil
}

// BuildAuthorizeURL returns the Melhor Envio OAuth authorize URL for the
// given state and scopes. Useful for the admin connection flow.
func BuildAuthorizeURL(env, clientID, redirectURI, state string, scopes []string) string {
	base := meSandboxBaseURL
	if env == meEnvProduction {
		base = meProdBaseURL
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("state", state)
	q.Set("scope", strings.Join(scopes, " "))
	return base + meAuthPath + "?" + q.Encode()
}

// ExchangeAuthorizationCode exchanges an authorization code for a token.
// Called once by the admin OAuth callback; the caller persists the credentials.
func ExchangeAuthorizationCode(ctx context.Context, env, clientID, clientSecret, redirectURI, code, userAgent string) (*Credentials, error) {
	base := meSandboxBaseURL
	if env == meEnvProduction {
		base = meProdBaseURL
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     clientID,
		"client_secret": clientSecret,
		"redirect_uri":  redirectURI,
		"code":          code,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling token body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+meTokenPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing token request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange code failed: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp struct {
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		expiresInSeconds = 30 * 24 * 3600
	}

	return &Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(expiresInSeconds) * time.Second),
	}, nil
}

// =============================================================================
// QUOTE
// =============================================================================

// meProductRequest is the per-product block in the calculate payload.
type meProductRequest struct {
	ID             string  `json:"id"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	Length         int     `json:"length"`
	Weight         float64 `json:"weight"`          // in kg
	InsuranceValue float64 `json:"insurance_value"` // in BRL (reais)
	Quantity       int     `json:"quantity"`
}

// meQuoteResponse is a single entry in the calculate response array.
type meQuoteResponse struct {
	ID                 int             `json:"id"`
	Name               string          `json:"name"`
	Price              json.RawMessage `json:"price"`         // "23.50" or 23.50
	CustomPrice        json.RawMessage `json:"custom_price"`  // same
	DeliveryTime       json.RawMessage `json:"delivery_time"` // int or { min, max }
	CustomDeliveryTime json.RawMessage `json:"custom_delivery_time"`
	Company            struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	} `json:"company"`
	Error string `json:"error"`
}

// Quote calculates shipping costs for a cart.
func (m *MelhorEnvio) Quote(ctx context.Context, req QuoteRequest) ([]QuoteOption, error) {
	if err := validateQuoteRequest(req); err != nil {
		return nil, err
	}

	products := make([]meProductRequest, 0, len(req.Items))
	heaviestIdx := 0
	var heaviest int
	for i, it := range req.Items {
		if it.WeightGrams > heaviest {
			heaviest = it.WeightGrams
			heaviestIdx = i
		}
		products = append(products, meProductRequest{
			ID:             nonEmptyString(it.ID, fmt.Sprintf("item-%d", i)),
			Width:          it.WidthCm,
			Height:         it.HeightCm,
			Length:         it.LengthCm,
			Weight:         kilogramsFromGrams(it.WeightGrams),
			InsuranceValue: reaisFromCents(it.InsuranceValueCents),
			Quantity:       it.Quantity,
		})
	}

	// Add the consolidating package weight to the heaviest item as a naive
	// approximation. See the plan for context; a real packing algorithm can
	// replace this later.
	if req.ExtraPackageWeightGrams > 0 && len(products) > 0 {
		products[heaviestIdx].Weight += kilogramsFromGrams(req.ExtraPackageWeightGrams)
	}

	body := map[string]any{
		"from":     map[string]string{"postal_code": sanitizeZip(req.FromZip)},
		"to":       map[string]string{"postal_code": sanitizeZip(req.ToZip)},
		"products": products,
		"options": map[string]any{
			"receipt":  req.Receipt,
			"own_hand": req.OwnHand,
		},
	}
	if len(req.ServiceIDs) > 0 {
		// Melhor Envio accepts comma-separated numeric ids. Filter out any
		// entries we cannot parse as int (they are not ours).
		valid := make([]string, 0, len(req.ServiceIDs))
		for _, raw := range req.ServiceIDs {
			if _, err := strconv.Atoi(raw); err == nil {
				valid = append(valid, raw)
			}
		}
		if len(valid) > 0 {
			body["services"] = strings.Join(valid, ",")
		}
	}

	respBody, err := m.doAuthenticated(ctx, http.MethodPost, meCalculatePath, body)
	if err != nil {
		return nil, err
	}

	results, err := parseQuoteResults(respBody)
	if err != nil {
		return nil, fmt.Errorf("parsing quote response: %w, body=%s", err, string(respBody))
	}

	options := make([]QuoteOption, 0, len(results))
	for _, r := range results {
		opt := QuoteOption{
			Provider:    providers.ProviderMelhorEnvio,
			ServiceID:   strconv.Itoa(r.ID),
			Service:     r.Name,
			Carrier:     r.Company.Name,
			CarrierLogo: r.Company.Picture,
			Available:   r.Error == "",
			Error:       r.Error,
		}
		if opt.Available {
			price := parseFlexibleFloat(r.Price)
			if customPrice := parseFlexibleFloat(r.CustomPrice); customPrice > 0 {
				price = customPrice
			}
			opt.PriceCents = int64(math.Round(price * 100))
			opt.DeadlineDays = parseDeliveryTime(r.DeliveryTime, r.CustomDeliveryTime)
		}
		options = append(options, opt)
	}

	return options, nil
}

// ListCarriers returns the carriers available for the authenticated account.
func (m *MelhorEnvio) ListCarriers(ctx context.Context) ([]CarrierService, error) {
	respBody, err := m.doAuthenticated(ctx, http.MethodGet, meCompaniesPath, nil)
	if err != nil {
		return nil, err
	}

	var companies []struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		Picture  string `json:"picture"`
		Services []struct {
			ID           int    `json:"id"`
			Name         string `json:"name"`
			Restrictions struct {
				InsuranceValue struct {
					Min float64 `json:"min"`
					Max float64 `json:"max"`
				} `json:"insurance_value"`
			} `json:"restrictions"`
		} `json:"services"`
	}
	if err := json.Unmarshal(respBody, &companies); err != nil {
		return nil, fmt.Errorf("parsing companies response: %w", err)
	}

	out := make([]CarrierService, 0)
	for _, c := range companies {
		for _, s := range c.Services {
			out = append(out, CarrierService{
				ServiceID:         strconv.Itoa(s.ID),
				Service:           s.Name,
				Carrier:           c.Name,
				CarrierLogo:       c.Picture,
				InsuranceMaxCents: int64(math.Round(s.Restrictions.InsuranceValue.Max * 100)),
			})
		}
	}
	return out, nil
}

// =============================================================================
// SHIPMENT LIFECYCLE — implements providers.ShippingOrderProvider.
// =============================================================================
//
// Melhor Envio splits creating a label into four calls:
//   1) POST /me/cart            — adds the shipment to the cart (CreateShipment)
//   2) POST /me/shipment/checkout — pays the freight using the account balance
//   3) POST /me/shipment/generate — authorises with the carrier (Correios, …)
//   4) POST /me/shipment/print    — returns a downloadable label URL
//
// We expose (1) on CreateShipment and pipeline (2)+(3)+(4) under
// GenerateLabels because the merchant is expected to "Criar envio" first
// (which leaves the order paid by the buyer but not yet at the carrier) and
// then click "Gerar etiqueta" once the NFe is attached. This mirrors the
// SmartEnvios surface and the existing OrderLogistics frontend.

// CreateShipment posts the order to Melhor Envio's cart. The returned id is
// what every follow-up call (checkout/generate/print/tracking/cancel) takes
// as the order identifier — we persist it as ProviderOrderID.
func (m *MelhorEnvio) CreateShipment(ctx context.Context, req CreateShipmentRequest) (*CreateShipmentResult, error) {
	if req.QuoteServiceID == "" {
		return nil, fmt.Errorf("quote_service_id is required")
	}
	if req.Sender.ZipCode == "" || req.Destiny.ZipCode == "" {
		return nil, fmt.Errorf("sender and destiny zip codes are required")
	}
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("at least one item is required")
	}

	serviceID, err := strconv.Atoi(strings.TrimSpace(req.QuoteServiceID))
	if err != nil {
		return nil, fmt.Errorf("invalid quote_service_id %q: must be the integer service id from /shipment/calculate", req.QuoteServiceID)
	}

	products, totalGrams, totalCents := buildMECartProducts(req.Items)
	volumes := buildMECartVolumes(req.Items, totalGrams)

	// ME's "options.invoice.key" is the documented home for the chave NFe.
	// When absent the shipment ships as a Declaração de Conteúdo and the
	// merchant can re-link the NFe later via the cart edit endpoint.
	options := meCartOptions{
		InsuranceValue: math.Round(reaisFromCents(totalCents)*100) / 100,
		Receipt:        false,
		OwnHand:        false,
		Reverse:        false,
		NonCommercial:  false,
	}
	if req.InvoiceKey != "" {
		options.Invoice = &meCartInvoice{Key: strings.TrimSpace(req.InvoiceKey)}
	}
	if req.Observation != "" {
		options.Note = req.Observation
	}

	body := meCartCreateRequest{
		Service:  serviceID,
		From:     toMECartAddress(req.Sender),
		To:       toMECartAddress(req.Destiny),
		Products: products,
		Volumes:  volumes,
		Options:  options,
	}

	respBody, err := m.doAuthenticated(ctx, http.MethodPost, meCartPath, body)
	if err != nil {
		return nil, err
	}

	var parsed meCartCreateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing me cart response: %w, body=%s", err, string(respBody))
	}

	var meta map[string]any
	_ = json.Unmarshal(respBody, &meta)
	createdAt, _ := time.Parse(time.RFC3339, parsed.CreatedAt)
	if createdAt.IsZero() {
		// ME sometimes returns "2025-05-09 12:34:56" without TZ.
		createdAt, _ = time.Parse("2006-01-02 15:04:05", parsed.CreatedAt)
	}

	return &CreateShipmentResult{
		ProviderOrderID:     parsed.ID,
		ProviderOrderNumber: parsed.Protocol,
		TrackingCode:        parsed.Tracking,
		InvoiceID:           "",
		Status:              mapMelhorEnvioStatus(parsed.Status),
		StatusRawCode:       0,
		StatusRawName:       parsed.Status,
		CreatedAt:           createdAt,
		ProviderMeta:        meta,
	}, nil
}

// AttachInvoice sets the NFe key on an existing cart row. Melhor Envio doesn't
// document a dedicated "attach invoice" endpoint — the chave de acesso lives
// on options.invoice and can be edited via PUT /api/v2/me/cart/{id} as long
// as the shipment is still in the "pending" state (i.e. before checkout).
// We surface ErrOperationNotSupported when no key is provided so the caller
// knows we don't accept "clear the NFe" via this method.
func (m *MelhorEnvio) AttachInvoice(ctx context.Context, req AttachInvoiceRequest) error {
	if req.InvoiceKey == "" {
		return fmt.Errorf("invoice_key is required")
	}
	if req.ProviderOrderID == "" {
		return fmt.Errorf("provider_order_id is required")
	}
	body := map[string]any{
		"options": map[string]any{
			"invoice": map[string]any{"key": req.InvoiceKey},
		},
	}
	if _, err := m.doAuthenticated(ctx, http.MethodPut, meCartPath+"/"+req.ProviderOrderID, body); err != nil {
		return fmt.Errorf("attaching invoice key on melhor envio cart %s: %w", req.ProviderOrderID, err)
	}
	return nil
}

// UploadInvoiceXML — Melhor Envio doesn't accept the raw NFe XML; the chave
// de acesso is enough for the carriers it integrates with. We return
// ErrOperationNotSupported so the admin UI can hide the "Upload XML" action
// for this provider instead of failing silently.
func (m *MelhorEnvio) UploadInvoiceXML(ctx context.Context, req UploadInvoiceXMLRequest) error {
	return providers.ErrOperationNotSupported
}

// GenerateLabels runs the full pay → authorise → print pipeline. All three
// calls take the same `{orders: [id, …]}` body and Melhor Envio is OK with
// re-running checkout/generate when they're already done (it's idempotent
// per shipment), so a partial earlier failure is recoverable by clicking
// the button again.
func (m *MelhorEnvio) GenerateLabels(ctx context.Context, req GenerateLabelsRequest) (*GenerateLabelsResult, error) {
	orderIDs := req.ProviderOrderIDs
	if len(orderIDs) == 0 {
		return nil, fmt.Errorf("at least one provider_order_id is required")
	}

	// 1) Checkout (paga o frete usando o saldo da conta)
	if _, err := m.doAuthenticated(ctx, http.MethodPost, meCheckoutPath, map[string]any{"orders": orderIDs}); err != nil {
		return nil, fmt.Errorf("melhor envio checkout: %w", err)
	}

	// 2) Generate (autoriza com a transportadora — gera tracking e dispatch)
	generateBody, err := m.doAuthenticated(ctx, http.MethodPost, meGeneratePath, map[string]any{"orders": orderIDs})
	if err != nil {
		return nil, fmt.Errorf("melhor envio generate: %w", err)
	}
	// Per-order generate metadata. Carries authorization_code + tracking which
	// we mirror onto the response so the caller can refresh the local row.
	var generateMeta map[string]meGenerateOrder
	_ = json.Unmarshal(generateBody, &generateMeta)

	// 3) Print (URL de download da etiqueta)
	mode := "private"
	if req.Format != "" {
		// ME does not let us swap pdf/zpl format on this endpoint — caller
		// keeps "pdf" implicit and a zpl-only future will move to /imprimir/dace.
		_ = req.Format
	}
	printBody, err := m.doAuthenticated(ctx, http.MethodPost, mePrintPath, map[string]any{
		"mode":   mode,
		"orders": orderIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("melhor envio print: %w", err)
	}
	var printResp struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(printBody, &printResp)

	tickets := make([]LabelTicket, 0, len(orderIDs))
	for _, id := range orderIDs {
		t := LabelTicket{ProviderOrderID: id}
		if meta, ok := generateMeta[id]; ok {
			t.TrackingCode = meta.Tracking
			t.PublicTracking = "https://www.melhorrastreio.com.br/rastreio/" + meta.Tracking
		}
		tickets = append(tickets, t)
	}
	return &GenerateLabelsResult{
		LabelURL: printResp.URL,
		Tickets:  tickets,
	}, nil
}

// TrackShipment pulls the current status + history for a Melhor Envio order.
// Tracking lookup accepts the order id (a uuid) under POST /shipment/tracking
// — the GET version with `orders[]=` works too but POST handles >1 id more
// robustly; here we always send exactly one.
func (m *MelhorEnvio) TrackShipment(ctx context.Context, req TrackShipmentRequest) (*TrackShipmentResult, error) {
	id := req.ProviderOrderID
	if id == "" {
		id = req.TrackingCode
	}
	if id == "" {
		return nil, fmt.Errorf("provider_order_id or tracking_code is required")
	}

	respBody, err := m.doAuthenticated(ctx, http.MethodPost, meTrackingPath, map[string]any{"orders": []string{id}})
	if err != nil {
		return nil, err
	}

	// Response is keyed by order id: { "<id>": { ...meTrackingOrder } }.
	var keyed map[string]meTrackingOrder
	if err := json.Unmarshal(respBody, &keyed); err != nil {
		return nil, fmt.Errorf("parsing me tracking response: %w, body=%s", err, string(respBody))
	}
	order, ok := keyed[id]
	if !ok {
		// Fallback: ME sometimes returns the order keyed by tracking code.
		for _, v := range keyed {
			order = v
			break
		}
	}

	out := &TrackShipmentResult{
		TrackingCode:  order.Tracking,
		Carrier:       order.Service.Company,
		Service:       order.Service.Name,
		CurrentStatus: mapMelhorEnvioStatus(order.Status),
	}

	for _, ev := range order.TrackingHistory {
		ts, _ := time.Parse(time.RFC3339, ev.OccurredAt)
		if ts.IsZero() {
			ts, _ = time.Parse("2006-01-02 15:04:05", ev.OccurredAt)
		}
		out.Events = append(out.Events, TrackingEvent{
			Status:      mapMelhorEnvioStatus(ev.Status),
			RawCode:     0,
			RawName:     ev.Status,
			Observation: ev.Description,
			EventAt:     ts,
		})
	}

	var meta map[string]any
	_ = json.Unmarshal(respBody, &meta)
	out.ProviderMeta = meta
	return out, nil
}

// =============================================================================
// MELHOR ENVIO — request/response shapes
// =============================================================================

type meCartCreateRequest struct {
	Service  int             `json:"service"`
	From     meCartAddress   `json:"from"`
	To       meCartAddress   `json:"to"`
	Products []meCartProduct `json:"products"`
	Volumes  []meCartVolume  `json:"volumes"`
	Options  meCartOptions   `json:"options"`
}

type meCartAddress struct {
	Name            string `json:"name"`
	Phone           string `json:"phone,omitempty"`
	Email           string `json:"email,omitempty"`
	Document        string `json:"document,omitempty"`
	CompanyDocument string `json:"company_document,omitempty"`
	Address         string `json:"address"`
	Complement      string `json:"complement,omitempty"`
	Number          string `json:"number"`
	District        string `json:"district"`
	City            string `json:"city,omitempty"`
	StateAbbr       string `json:"state_abbr,omitempty"`
	CountryID       string `json:"country_id"`
	PostalCode      string `json:"postal_code"`
	Note            string `json:"note,omitempty"`
}

type meCartProduct struct {
	Name         string  `json:"name"`
	Quantity     int     `json:"quantity"`
	UnitaryValue float64 `json:"unitary_value"`
	Weight       float64 `json:"weight,omitempty"`
}

type meCartVolume struct {
	Height float64 `json:"height"`
	Width  float64 `json:"width"`
	Length float64 `json:"length"`
	Weight float64 `json:"weight"`
}

type meCartOptions struct {
	InsuranceValue float64        `json:"insurance_value"`
	Receipt        bool           `json:"receipt"`
	OwnHand        bool           `json:"own_hand"`
	Reverse        bool           `json:"reverse"`
	NonCommercial  bool           `json:"non_commercial"`
	Invoice        *meCartInvoice `json:"invoice,omitempty"`
	Note           string         `json:"note,omitempty"`
}

type meCartInvoice struct {
	Key string `json:"key"`
}

type meCartCreateResponse struct {
	ID        string `json:"id"`
	Protocol  string `json:"protocol"`
	Status    string `json:"status"`
	Tracking  string `json:"tracking"`
	CreatedAt string `json:"created_at"`
}

type meGenerateOrder struct {
	AuthorizationCode string `json:"authorization_code"`
	Tracking          string `json:"tracking"`
	Status            string `json:"status"`
}

type meTrackingOrder struct {
	ID              string                  `json:"id"`
	Status          string                  `json:"status"`
	Tracking        string                  `json:"tracking"`
	Service         meTrackingService       `json:"service"`
	TrackingHistory []meTrackingHistoryItem `json:"tracking_history"`
}

type meTrackingService struct {
	Name    string `json:"name"`
	Company string `json:"company"`
}

type meTrackingHistoryItem struct {
	Status      string `json:"status"`
	Description string `json:"description"`
	OccurredAt  string `json:"occurred_at"`
}

func toMECartAddress(p providers.ShippingAddressPoint) meCartAddress {
	out := meCartAddress{
		Name:       p.Name,
		Phone:      p.Phone,
		Email:      p.Email,
		Address:    p.Street,
		Complement: p.Complement,
		Number:     nonEmptyString(p.Number, "S/N"),
		District:   p.Neighborhood,
		City:       p.City,
		StateAbbr:  p.State,
		CountryID:  "BR",
		PostalCode: sanitizeZip(ShippingZip(p.ZipCode)),
		Note:       p.Observation,
	}
	doc := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, p.Document)
	switch len(doc) {
	case 14:
		out.CompanyDocument = doc
	case 11:
		out.Document = doc
	default:
		// Surface the raw value so the carrier sees something — ME validates
		// and will reject explicitly if the doc is malformed.
		out.Document = doc
	}
	return out
}

func buildMECartProducts(items []ShippingItem) ([]meCartProduct, int, int64) {
	products := make([]meCartProduct, 0, len(items))
	totalGrams := 0
	totalCents := int64(0)
	for _, it := range items {
		qty := it.Quantity
		if qty <= 0 {
			qty = 1
		}
		products = append(products, meCartProduct{
			Name:         nonEmptyString(it.Name, "Produto"),
			Quantity:     qty,
			UnitaryValue: reaisFromCents(it.UnitPriceCents),
			Weight:       kilogramsFromGrams(it.WeightGrams),
		})
		totalGrams += it.WeightGrams * qty
		totalCents += it.UnitPriceCents * int64(qty)
	}
	return products, totalGrams, totalCents
}

func buildMECartVolumes(items []ShippingItem, totalGrams int) []meCartVolume {
	// Single consolidated volume — Melhor Envio expects per-package volumes,
	// which mirrors the freight quote we ran. Multi-package shipments are out
	// of scope for now (matches SmartEnvios behaviour).
	first := items[0]
	w := kilogramsFromGrams(totalGrams)
	if w <= 0 {
		w = kilogramsFromGrams(first.WeightGrams)
	}
	return []meCartVolume{{
		Height: float64(first.HeightCm),
		Width:  float64(first.WidthCm),
		Length: float64(first.LengthCm),
		Weight: w,
	}}
}

// mapMelhorEnvioStatus translates ME's lifecycle states to LiveCart's
// normalised TrackingStatus enum. ME states (per /reagindo-a-estados):
//
//	pending   → buyer hasn't paid the freight
//	released  → paid, awaiting authorisation with carrier
//	posted    → at the carrier, in transit
//	delivered, cancelled, undelivered, suspended.
func mapMelhorEnvioStatus(raw string) TrackingStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pending":
		return providers.TrackingStatusPending
	case "released":
		return providers.TrackingStatusAwaitingPickup
	case "posted":
		return providers.TrackingStatusInTransit
	case "delivered":
		return providers.TrackingStatusDelivered
	case "cancelled", "canceled":
		return providers.TrackingStatusCanceled
	case "undelivered":
		return providers.TrackingStatusNotDelivered
	case "suspended":
		return providers.TrackingStatusShipmentBlocked
	case "expired":
		return providers.TrackingStatusIssue
	default:
		return providers.TrackingStatusUnknown
	}
}

// =============================================================================
// HELPERS
// =============================================================================

// doAuthenticated performs an authenticated request with Melhor Envio headers.
func (m *MelhorEnvio) doAuthenticated(ctx context.Context, method, path string, body any) ([]byte, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + m.credentials.AccessToken,
		"User-Agent":    m.userAgent,
	}
	resp, respBody, err := m.DoRequest(ctx, method, m.baseURL()+path, body, headers)
	if err != nil {
		return nil, err
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("melhor envio %s %s failed: status %d, body: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func validateQuoteRequest(req QuoteRequest) error {
	if sanitizeZip(req.FromZip) == "" {
		return fmt.Errorf("from_zip is required")
	}
	if sanitizeZip(req.ToZip) == "" {
		return fmt.Errorf("to_zip is required")
	}
	if len(req.Items) == 0 {
		return fmt.Errorf("at least one item is required")
	}
	for i, it := range req.Items {
		if it.Quantity <= 0 {
			return fmt.Errorf("item %d: quantity must be > 0", i)
		}
		if it.Quantity > 100 {
			return fmt.Errorf("item %d: quantity exceeds Melhor Envio limit of 100 units per product", i)
		}
		if it.WeightGrams <= 0 || it.HeightCm <= 0 || it.WidthCm <= 0 || it.LengthCm <= 0 {
			return fmt.Errorf("item %d: weight and dimensions must be positive", i)
		}
	}
	return nil
}

// parseQuoteResults accepts either an array of quote results (the documented
// shape) or a single result object — which Melhor Envio returns when the
// request filters down to one service and the body omits the wrapping array.
func parseQuoteResults(body []byte) ([]meQuoteResponse, error) {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var arr []meQuoteResponse
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var single meQuoteResponse
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	return []meQuoteResponse{single}, nil
}

func sanitizeZip(z ShippingZip) string {
	var b strings.Builder
	for _, r := range string(z) {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func nonEmptyString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func kilogramsFromGrams(g int) float64 {
	return math.Round(float64(g)) / 1000.0
}

func reaisFromCents(cents int64) float64 {
	return float64(cents) / 100.0
}

// parseFlexibleFloat handles "23.50" (string) or 23.50 (number) in Melhor Envio responses.
func parseFlexibleFloat(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	trimmed := strings.Trim(string(raw), "\"")
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseDeliveryTime handles int / { min, max } / string shapes.
// Prefers custom_delivery_time.max when present.
func parseDeliveryTime(normal, custom json.RawMessage) int {
	if days := extractDeliveryMax(custom); days > 0 {
		return days
	}
	return extractDeliveryMax(normal)
}

func extractDeliveryMax(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	// Try as int first.
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	// Then as object with min/max.
	var obj struct {
		Min int `json:"min"`
		Max int `json:"max"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Max > 0 {
			return obj.Max
		}
		return obj.Min
	}
	// Fallback: as string number.
	trimmed := strings.Trim(string(raw), "\"")
	if v, err := strconv.Atoi(trimmed); err == nil {
		return v
	}
	return 0
}

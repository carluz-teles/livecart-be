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
)

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
			"env":          m.env,
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
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	Price        json.RawMessage `json:"price"`         // "23.50" or 23.50
	CustomPrice  json.RawMessage `json:"custom_price"`  // same
	DeliveryTime json.RawMessage `json:"delivery_time"` // int or { min, max }
	CustomDeliveryTime json.RawMessage `json:"custom_delivery_time"`
	Company      struct {
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
		"from": map[string]string{"postal_code": sanitizeZip(req.FromZip)},
		"to":   map[string]string{"postal_code": sanitizeZip(req.ToZip)},
		"products": products,
		"options": map[string]any{
			"receipt":  req.Receipt,
			"own_hand": req.OwnHand,
		},
	}
	if len(req.ServiceIDs) > 0 {
		strs := make([]string, len(req.ServiceIDs))
		for i, id := range req.ServiceIDs {
			strs[i] = strconv.Itoa(id)
		}
		body["services"] = strings.Join(strs, ",")
	}

	respBody, err := m.doAuthenticated(ctx, http.MethodPost, meCalculatePath, body)
	if err != nil {
		return nil, err
	}

	var results []meQuoteResponse
	if err := json.Unmarshal(respBody, &results); err != nil {
		return nil, fmt.Errorf("parsing quote response: %w, body=%s", err, string(respBody))
	}

	options := make([]QuoteOption, 0, len(results))
	for _, r := range results {
		opt := QuoteOption{
			ServiceID:   r.ID,
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
				ServiceID:         s.ID,
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

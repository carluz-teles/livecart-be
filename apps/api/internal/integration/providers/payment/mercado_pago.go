package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

const (
	mpAPIBaseURL = "https://api.mercadopago.com"
	mpOAuthURL   = "https://api.mercadopago.com/oauth/token"
)

// MercadoPago implements the PaymentProvider interface for Mercado Pago.
type MercadoPago struct {
	*providers.BaseProvider
	credentials *Credentials
	appID       string
	appSecret   string
}

// MercadoPagoConfig contains configuration for the Mercado Pago provider.
type MercadoPagoConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	AppID         string
	AppSecret     string
	Logger        *zap.Logger
	LogFunc       providers.LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// NewMercadoPago creates a new Mercado Pago provider.
func NewMercadoPago(cfg MercadoPagoConfig) (*MercadoPago, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("credentials are required")
	}
	if cfg.Credentials.AccessToken == "" {
		return nil, fmt.Errorf("access_token is required")
	}

	return &MercadoPago{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       30 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		credentials: cfg.Credentials,
		appID:       cfg.AppID,
		appSecret:   cfg.AppSecret,
	}, nil
}

// Type returns the provider type.
func (m *MercadoPago) Type() providers.ProviderType {
	return providers.ProviderTypePayment
}

// Name returns the provider name.
func (m *MercadoPago) Name() providers.ProviderName {
	return providers.ProviderMercadoPago
}

// ValidateCredentials validates the current credentials by calling the API.
func (m *MercadoPago) ValidateCredentials(ctx context.Context) error {
	url := mpAPIBaseURL + "/users/me"
	headers := m.authHeaders()

	resp, _, err := m.DoRequest(ctx, http.MethodGet, url, nil, headers)
	if err != nil {
		return fmt.Errorf("validating credentials: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid credentials: status %d", resp.StatusCode)
	}
	return nil
}

// TestConnection tests the connection to Mercado Pago API.
func (m *MercadoPago) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	url := mpAPIBaseURL + "/users/me"
	headers := m.authHeaders()

	resp, body, err := m.DoRequest(ctx, http.MethodGet, url, nil, headers)
	latency := time.Since(start)

	result := &providers.TestConnectionResult{
		Latency:  latency,
		TestedAt: time.Now(),
	}

	if err != nil {
		result.Success = false
		result.Message = fmt.Sprintf("Falha na conexão: %v", err)
		return result, nil
	}

	if resp.StatusCode == http.StatusUnauthorized {
		result.Success = false
		result.Message = "Token inválido ou expirado"
		return result, nil
	}

	if resp.StatusCode != http.StatusOK {
		result.Success = false
		result.Message = fmt.Sprintf("Erro na API: status %d", resp.StatusCode)
		return result, nil
	}

	// Parse user info
	var userInfo struct {
		ID        int64  `json:"id"`
		Nickname  string `json:"nickname"`
		Email     string `json:"email"`
		SiteID    string `json:"site_id"`
		CountryID string `json:"country_id"`
	}
	if err := json.Unmarshal(body, &userInfo); err == nil {
		result.AccountInfo = map[string]any{
			"user_id":  userInfo.ID,
			"nickname": userInfo.Nickname,
			"email":    userInfo.Email,
			"site_id":  userInfo.SiteID,
			"country":  userInfo.CountryID,
		}
	}

	result.Success = true
	result.Message = "Conexão estabelecida com sucesso"
	return result, nil
}

// RefreshToken refreshes the OAuth access token.
func (m *MercadoPago) RefreshToken(ctx context.Context) (*Credentials, error) {
	if m.credentials.RefreshToken == "" {
		return nil, nil // No refresh token available
	}
	if m.appID == "" || m.appSecret == "" {
		return nil, fmt.Errorf("app credentials required for token refresh")
	}

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     m.appID,
		"client_secret": m.appSecret,
		"refresh_token": m.credentials.RefreshToken,
	}

	resp, body, err := m.DoRequest(ctx, http.MethodPost, mpOAuthURL, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("refreshing token: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("token refresh failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	return &Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}, nil
}

// CreateCheckout creates a checkout preference in Mercado Pago.
func (m *MercadoPago) CreateCheckout(ctx context.Context, order CheckoutOrder) (*CheckoutResult, error) {
	url := mpAPIBaseURL + "/checkout/preferences"

	// Build items array
	items := make([]map[string]any, len(order.Items))
	for i, item := range order.Items {
		items[i] = map[string]any{
			"id":          item.ID,
			"title":       item.Name,
			"description": item.Description,
			"quantity":    item.Quantity,
			"unit_price":  float64(item.UnitPrice) / 100, // Convert cents to currency units
			"picture_url": item.ImageURL,
			"currency_id": order.Currency,
		}
	}

	// Build payer object
	payer := map[string]any{
		"email": order.Customer.Email,
	}
	if order.Customer.Name != "" {
		payer["name"] = order.Customer.Name
	}
	if order.Customer.Phone != "" {
		payer["phone"] = map[string]string{
			"number": order.Customer.Phone,
		}
	}
	if order.Customer.Document != "" {
		payer["identification"] = map[string]string{
			"type":   "CPF",
			"number": order.Customer.Document,
		}
	}

	payload := map[string]any{
		"items":              items,
		"payer":              payer,
		"external_reference": order.ExternalID,
		"back_urls": map[string]string{
			"success": order.SuccessURL,
			"failure": order.FailureURL,
			"pending": order.SuccessURL,
		},
		"auto_return":      "approved",
		"notification_url": order.NotifyURL,
	}

	if order.Metadata != nil {
		payload["metadata"] = order.Metadata
	}

	if order.ExpiresIn != nil {
		expiration := time.Now().Add(*order.ExpiresIn)
		payload["expiration_date_to"] = expiration.Format(time.RFC3339)
	}

	resp, body, err := m.DoRequest(ctx, http.MethodPost, url, payload, m.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("creating checkout: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("create checkout failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var mpResp struct {
		ID               string `json:"id"`
		InitPoint        string `json:"init_point"`
		SandboxInitPoint string `json:"sandbox_init_point"`
		DateOfExpiration string `json:"date_of_expiration,omitempty"`
	}
	if err := json.Unmarshal(body, &mpResp); err != nil {
		return nil, fmt.Errorf("parsing checkout response: %w", err)
	}

	var expiresAt *time.Time
	if mpResp.DateOfExpiration != "" {
		if t, err := time.Parse(time.RFC3339, mpResp.DateOfExpiration); err == nil {
			expiresAt = &t
		}
	}

	// Use sandbox URL in development
	checkoutURL := mpResp.InitPoint
	if checkoutURL == "" {
		checkoutURL = mpResp.SandboxInitPoint
	}

	return &CheckoutResult{
		CheckoutID:  mpResp.ID,
		CheckoutURL: checkoutURL,
		ExpiresAt:   expiresAt,
	}, nil
}

// GetPaymentStatus retrieves the status of a payment.
func (m *MercadoPago) GetPaymentStatus(ctx context.Context, paymentID string) (*PaymentStatus, error) {
	url := fmt.Sprintf("%s/v1/payments/%s", mpAPIBaseURL, paymentID)

	resp, body, err := m.DoRequest(ctx, http.MethodGet, url, nil, m.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting payment: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("payment not found: %s", paymentID)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get payment failed: status %d", resp.StatusCode)
	}

	var mpPayment struct {
		ID                int64          `json:"id"`
		Status            string         `json:"status"`
		StatusDetail      string         `json:"status_detail"`
		TransactionAmount float64        `json:"transaction_amount"`
		CurrencyID        string         `json:"currency_id"`
		DateApproved      string         `json:"date_approved,omitempty"`
		DateCreated       string         `json:"date_created"`
		Metadata          map[string]any `json:"metadata"`
		ExternalReference string         `json:"external_reference"`
	}
	if err := json.Unmarshal(body, &mpPayment); err != nil {
		return nil, fmt.Errorf("parsing payment response: %w", err)
	}

	status := mapMPStatus(mpPayment.Status)

	var paidAt *time.Time
	if mpPayment.DateApproved != "" {
		if t, err := time.Parse(time.RFC3339, mpPayment.DateApproved); err == nil {
			paidAt = &t
		}
	}

	return &PaymentStatus{
		PaymentID:         fmt.Sprintf("%d", mpPayment.ID),
		Status:            status,
		Amount:            int64(mpPayment.TransactionAmount * 100), // Convert to cents
		PaidAt:            paidAt,
		FailureReason:     mpPayment.StatusDetail,
		Metadata:          mpPayment.Metadata,
		ExternalReference: mpPayment.ExternalReference,
	}, nil
}

// RefundPayment initiates a refund for a payment.
func (m *MercadoPago) RefundPayment(ctx context.Context, paymentID string, amount *int64) (*RefundResult, error) {
	url := fmt.Sprintf("%s/v1/payments/%s/refunds", mpAPIBaseURL, paymentID)

	var payload map[string]any
	if amount != nil {
		payload = map[string]any{
			"amount": float64(*amount) / 100, // Convert cents to currency units
		}
	}

	resp, body, err := m.DoRequest(ctx, http.MethodPost, url, payload, m.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("refunding payment: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("refund failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var mpRefund struct {
		ID          int64   `json:"id"`
		Status      string  `json:"status"`
		Amount      float64 `json:"amount"`
		DateCreated string  `json:"date_created"`
	}
	if err := json.Unmarshal(body, &mpRefund); err != nil {
		return nil, fmt.Errorf("parsing refund response: %w", err)
	}

	createdAt, _ := time.Parse(time.RFC3339, mpRefund.DateCreated)

	return &RefundResult{
		RefundID:  fmt.Sprintf("%d", mpRefund.ID),
		Status:    mpRefund.Status,
		Amount:    int64(mpRefund.Amount * 100), // Convert to cents
		CreatedAt: createdAt,
	}, nil
}

// authHeaders returns the authorization headers for API requests.
func (m *MercadoPago) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + m.credentials.AccessToken,
	}
}

// mapMPStatus maps Mercado Pago status to our PaymentState.
func mapMPStatus(status string) PaymentState {
	switch status {
	case "approved":
		return PaymentApproved
	case "pending", "in_process", "in_mediation":
		return PaymentPending
	case "rejected":
		return PaymentRejected
	case "cancelled":
		return PaymentCancelled
	case "refunded":
		return PaymentRefunded
	default:
		return PaymentPending
	}
}

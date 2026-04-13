package payment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

const (
	pagarmeAPIBaseURL = "https://api.pagar.me/core/v5"
)

// Pagarme implements the PaymentProvider interface for Pagar.me.
type Pagarme struct {
	*providers.BaseProvider
	credentials *Credentials
}

// PagarmeConfig contains configuration for the Pagar.me provider.
type PagarmeConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	Logger        *zap.Logger
	LogFunc       providers.LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// NewPagarme creates a new Pagar.me provider.
func NewPagarme(cfg PagarmeConfig) (*Pagarme, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("credentials are required")
	}
	if cfg.Credentials.APIKey == "" {
		return nil, fmt.Errorf("api_key (secret_key) is required")
	}

	return &Pagarme{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       30 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		credentials: cfg.Credentials,
	}, nil
}

// Type returns the provider type.
func (p *Pagarme) Type() providers.ProviderType {
	return providers.ProviderTypePayment
}

// Name returns the provider name.
func (p *Pagarme) Name() providers.ProviderName {
	return providers.ProviderPagarme
}

// ValidateCredentials validates the current credentials by calling the API.
func (p *Pagarme) ValidateCredentials(ctx context.Context) error {
	// Pagar.me doesn't have a dedicated endpoint to validate credentials,
	// so we try to list customers (limited to 1) to verify the API key
	url := pagarmeAPIBaseURL + "/customers?size=1"

	resp, _, err := p.DoRequest(ctx, http.MethodGet, url, nil, p.authHeaders())
	if err != nil {
		return fmt.Errorf("validating credentials: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("invalid credentials: unauthorized")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid credentials: status %d", resp.StatusCode)
	}
	return nil
}

// TestConnection tests the connection to Pagar.me API.
func (p *Pagarme) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	url := pagarmeAPIBaseURL + "/customers?size=1"

	resp, _, err := p.DoRequest(ctx, http.MethodGet, url, nil, p.authHeaders())
	latency := time.Since(start)

	result := &providers.TestConnectionResult{
		Latency:  latency,
		TestedAt: time.Now(),
	}

	if err != nil {
		result.Success = false
		result.Message = fmt.Sprintf("Falha na conexao: %v", err)
		return result, nil
	}

	if resp.StatusCode == http.StatusUnauthorized {
		result.Success = false
		result.Message = "Chave de API invalida"
		return result, nil
	}

	if resp.StatusCode != http.StatusOK {
		result.Success = false
		result.Message = fmt.Sprintf("Erro na API: status %d", resp.StatusCode)
		return result, nil
	}

	result.Success = true
	result.Message = "Conexao estabelecida com sucesso"
	result.AccountInfo = map[string]any{
		"provider": "pagarme",
	}
	return result, nil
}

// RefreshToken refreshes OAuth tokens - Pagar.me uses API keys, so no refresh needed.
func (p *Pagarme) RefreshToken(ctx context.Context) (*Credentials, error) {
	// Pagar.me uses API keys, not OAuth tokens, so no refresh is needed
	return nil, nil
}

// CreateCheckout creates an order with checkout payment method in Pagar.me.
func (p *Pagarme) CreateCheckout(ctx context.Context, order CheckoutOrder) (*CheckoutResult, error) {
	url := pagarmeAPIBaseURL + "/orders"

	// Build items array
	items := make([]map[string]any, len(order.Items))
	for i, item := range order.Items {
		items[i] = map[string]any{
			"amount":      item.UnitPrice,      // Already in cents
			"description": item.Name,
			"quantity":    item.Quantity,
			"code":        item.ID,
		}
	}

	// Build customer object
	customer := map[string]any{
		"name":  order.Customer.Name,
		"email": order.Customer.Email,
		"type":  "individual",
	}
	if order.Customer.Document != "" {
		customer["document"] = order.Customer.Document
		customer["document_type"] = "cpf"
	}
	if order.Customer.Phone != "" {
		customer["phones"] = map[string]any{
			"mobile_phone": map[string]any{
				"country_code": "55",
				"area_code":    extractAreaCode(order.Customer.Phone),
				"number":       extractPhoneNumber(order.Customer.Phone),
			},
		}
	}

	// Calculate expiration time
	expiresInMinutes := 120 // Default: 2 hours
	if order.ExpiresIn != nil {
		expiresInMinutes = int(order.ExpiresIn.Minutes())
	}

	// Build checkout configuration
	checkoutConfig := map[string]any{
		"expires_in":               expiresInMinutes,
		"billing_address_editable": true,
		"customer_editable":        true,
		"accepted_payment_methods": []string{"credit_card", "pix", "boleto"},
		"success_url":              order.SuccessURL,
	}

	// Add credit card installment options (1x full amount)
	checkoutConfig["credit_card"] = map[string]any{
		"capture":              true,
		"statement_descriptor": "LIVECART",
		"installments": []map[string]any{
			{
				"number": 1,
				"total":  order.TotalAmount,
			},
		},
	}

	// Add PIX configuration
	checkoutConfig["pix"] = map[string]any{
		"expires_in": expiresInMinutes * 60, // PIX expiration in seconds
	}

	// Add Boleto configuration
	checkoutConfig["boleto"] = map[string]any{
		"bank":         "033", // Santander
		"instructions": "Pagar ate o vencimento",
		"due_at":       time.Now().Add(72 * time.Hour).Format(time.RFC3339),
	}

	// Build payload
	payload := map[string]any{
		"code":     order.ExternalID,
		"items":    items,
		"customer": customer,
		"payments": []map[string]any{
			{
				"amount":         order.TotalAmount,
				"payment_method": "checkout",
				"checkout":       checkoutConfig,
			},
		},
	}

	if order.Metadata != nil {
		payload["metadata"] = order.Metadata
	}

	resp, body, err := p.DoRequest(ctx, http.MethodPost, url, payload, p.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("creating checkout: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("create checkout failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var pgResp pagarmeOrderResponse
	if err := json.Unmarshal(body, &pgResp); err != nil {
		return nil, fmt.Errorf("parsing checkout response: %w", err)
	}

	// Get checkout URL from the response
	var checkoutURL string
	var checkoutID string
	var expiresAt *time.Time

	if len(pgResp.Checkouts) > 0 {
		checkoutURL = pgResp.Checkouts[0].PaymentURL
		checkoutID = pgResp.Checkouts[0].ID
		if pgResp.Checkouts[0].ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, pgResp.Checkouts[0].ExpiresAt); err == nil {
				expiresAt = &t
			}
		}
	}

	if checkoutURL == "" {
		return nil, fmt.Errorf("no checkout URL returned from Pagar.me")
	}

	return &CheckoutResult{
		CheckoutID:  checkoutID,
		CheckoutURL: checkoutURL,
		ExpiresAt:   expiresAt,
	}, nil
}

// GetPaymentStatus retrieves the status of a payment/order.
func (p *Pagarme) GetPaymentStatus(ctx context.Context, orderID string) (*PaymentStatus, error) {
	url := fmt.Sprintf("%s/orders/%s", pagarmeAPIBaseURL, orderID)

	resp, body, err := p.DoRequest(ctx, http.MethodGet, url, nil, p.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting order: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("order not found: %s", orderID)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get order failed: status %d", resp.StatusCode)
	}

	var pgOrder pagarmeOrderResponse
	if err := json.Unmarshal(body, &pgOrder); err != nil {
		return nil, fmt.Errorf("parsing order response: %w", err)
	}

	status := mapPagarmeStatus(pgOrder.Status)

	var paidAt *time.Time
	if len(pgOrder.Charges) > 0 && pgOrder.Charges[0].PaidAt != "" {
		if t, err := time.Parse(time.RFC3339, pgOrder.Charges[0].PaidAt); err == nil {
			paidAt = &t
		}
	}

	return &PaymentStatus{
		PaymentID:         pgOrder.ID,
		Status:            status,
		Amount:            int64(pgOrder.Amount),
		PaidAt:            paidAt,
		ExternalReference: pgOrder.Code,
		Metadata:          pgOrder.Metadata,
	}, nil
}

// RefundPayment initiates a refund for an order's charge.
func (p *Pagarme) RefundPayment(ctx context.Context, chargeID string, amount *int64) (*RefundResult, error) {
	url := fmt.Sprintf("%s/charges/%s", pagarmeAPIBaseURL, chargeID)

	var payload map[string]any
	if amount != nil {
		payload = map[string]any{
			"amount": *amount,
		}
	}

	resp, body, err := p.DoRequest(ctx, http.MethodDelete, url, payload, p.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("refunding charge: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("refund failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var pgCharge struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Amount    int    `json:"amount"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(body, &pgCharge); err != nil {
		return nil, fmt.Errorf("parsing refund response: %w", err)
	}

	updatedAt, _ := time.Parse(time.RFC3339, pgCharge.UpdatedAt)

	return &RefundResult{
		RefundID:  pgCharge.ID,
		Status:    pgCharge.Status,
		Amount:    int64(pgCharge.Amount),
		CreatedAt: updatedAt,
	}, nil
}

// authHeaders returns the authorization headers for API requests.
// Pagar.me uses Basic Auth with API key as username and empty password.
func (p *Pagarme) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Basic " + basicAuth(p.credentials.APIKey, ""),
	}
}

// basicAuth encodes credentials for Basic Authentication.
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64Encode(auth)
}

// base64Encode encodes a string to base64.
func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// Helper function to extract area code from phone number
func extractAreaCode(phone string) string {
	// Remove non-numeric characters
	cleaned := ""
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			cleaned += string(r)
		}
	}
	// Brazilian phone format: (XX) XXXXX-XXXX
	if len(cleaned) >= 2 {
		return cleaned[:2]
	}
	return "11" // Default to Sao Paulo area code
}

// Helper function to extract phone number without area code
func extractPhoneNumber(phone string) string {
	// Remove non-numeric characters
	cleaned := ""
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			cleaned += string(r)
		}
	}
	// Skip area code (first 2 digits)
	if len(cleaned) > 2 {
		return cleaned[2:]
	}
	return cleaned
}

// mapPagarmeStatus maps Pagar.me status to our PaymentState.
func mapPagarmeStatus(status string) PaymentState {
	switch status {
	case "paid":
		return PaymentApproved
	case "pending":
		return PaymentPending
	case "failed":
		return PaymentRejected
	case "canceled":
		return PaymentCancelled
	case "refunded", "chargedback":
		return PaymentRefunded
	default:
		return PaymentPending
	}
}

// =============================================================================
// PAGAR.ME API RESPONSE TYPES
// =============================================================================

type pagarmeOrderResponse struct {
	ID        string            `json:"id"`
	Code      string            `json:"code"`
	Amount    int               `json:"amount"`
	Currency  string            `json:"currency"`
	Status    string            `json:"status"`
	Closed    bool              `json:"closed"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Checkouts []pagarmeCheckout `json:"checkouts"`
	Charges   []pagarmeCharge   `json:"charges"`
	Metadata  map[string]any    `json:"metadata"`
}

type pagarmeCheckout struct {
	ID         string `json:"id"`
	Amount     int    `json:"amount"`
	Status     string `json:"status"`
	PaymentURL string `json:"payment_url"`
	SuccessURL string `json:"success_url"`
	ExpiresAt  string `json:"expires_at"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type pagarmeCharge struct {
	ID            string `json:"id"`
	Code          string `json:"code"`
	Amount        int    `json:"amount"`
	Status        string `json:"status"`
	PaymentMethod string `json:"payment_method"`
	PaidAt        string `json:"paid_at"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

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
	var paymentMethod string
	var installments int
	if len(pgOrder.Charges) > 0 {
		if pgOrder.Charges[0].PaidAt != "" {
			if t, err := time.Parse(time.RFC3339, pgOrder.Charges[0].PaidAt); err == nil {
				paidAt = &t
			}
		}
		// Extract payment method from charge
		paymentMethod = mapPagarmePaymentMethod(pgOrder.Charges[0].PaymentMethod)
		if pgOrder.Charges[0].LastTransaction != nil {
			installments = pgOrder.Charges[0].LastTransaction.Installments
		}
	}

	return &PaymentStatus{
		PaymentID:         pgOrder.ID,
		Status:            status,
		Amount:            int64(pgOrder.Amount),
		PaidAt:            paidAt,
		ExternalReference: pgOrder.Code,
		Metadata:          pgOrder.Metadata,
		PaymentMethod:     paymentMethod,
		Installments:      installments,
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

// mapPagarmePaymentMethod maps Pagar.me payment_method to our payment method.
func mapPagarmePaymentMethod(method string) string {
	switch method {
	case "credit_card":
		return "credit_card"
	case "debit_card":
		return "debit_card"
	case "pix":
		return "pix"
	case "boleto":
		return "boleto"
	default:
		return "other"
	}
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
// TRANSPARENT CHECKOUT METHODS
// =============================================================================

// GetPublicKey returns the public key for client-side tokenization.
func (p *Pagarme) GetPublicKey(ctx context.Context) (string, error) {
	// For Pagar.me, the public key should be stored in credentials.Extra
	if p.credentials.Extra != nil {
		if pk, ok := p.credentials.Extra["public_key"].(string); ok && pk != "" {
			return pk, nil
		}
	}

	// If APISecret is provided, it might be the public key
	// Pagar.me uses "pk_test_xxx" for public keys and "sk_test_xxx" for secret keys
	if p.credentials.APISecret != "" {
		// Check if it's a public key format
		if len(p.credentials.APISecret) > 3 && p.credentials.APISecret[:3] == "pk_" {
			return p.credentials.APISecret, nil
		}
	}

	return "", fmt.Errorf("public key not available. Please configure the Pagar.me public key")
}

// ProcessCardPayment processes a payment with a tokenized card.
func (p *Pagarme) ProcessCardPayment(ctx context.Context, input CardPaymentInput) (*CardPaymentResult, error) {
	url := pagarmeAPIBaseURL + "/orders"

	// Build items array
	items := make([]map[string]any, len(input.Items))
	for i, item := range input.Items {
		items[i] = map[string]any{
			"amount":      item.UnitPrice, // Already in cents
			"description": item.Name,
			"quantity":    item.Quantity,
			"code":        item.ID,
		}
	}

	// Build customer object
	customer := map[string]any{
		"name":  input.Customer.Name,
		"email": input.Customer.Email,
		"type":  "individual",
	}
	if input.Customer.Document != "" {
		customer["document"] = input.Customer.Document
		customer["document_type"] = "cpf"
	}
	if input.Customer.Phone != "" {
		customer["phones"] = map[string]any{
			"mobile_phone": map[string]any{
				"country_code": "55",
				"area_code":    extractAreaCode(input.Customer.Phone),
				"number":       extractPhoneNumber(input.Customer.Phone),
			},
		}
	}

	// Build credit card payment
	cardPayment := map[string]any{
		"payment_method": "credit_card",
		"credit_card": map[string]any{
			"card_token":           input.Token,
			"installments":         input.Installments,
			"statement_descriptor": "LIVECART",
			"capture":              true,
		},
	}

	// Build payload
	payload := map[string]any{
		"code":     input.CartID,
		"items":    items,
		"customer": customer,
		"payments": []map[string]any{cardPayment},
	}

	if input.Metadata != nil {
		payload["metadata"] = input.Metadata
	}

	// Add idempotency key header
	headers := p.authHeaders()
	headers["X-Idempotency-Key"] = fmt.Sprintf("card-%s-%d", input.CartID, input.TotalAmount)

	resp, body, err := p.DoRequest(ctx, http.MethodPost, url, payload, headers)
	if err != nil {
		return nil, fmt.Errorf("processing card payment: %w", err)
	}

	var pgResp struct {
		ID       string          `json:"id"`
		Code     string          `json:"code"`
		Amount   int             `json:"amount"`
		Status   string          `json:"status"`
		Charges  []pagarmeCharge `json:"charges"`
		Metadata map[string]any  `json:"metadata"`
		Errors   []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &pgResp); err != nil {
		return nil, fmt.Errorf("parsing payment response: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		errMsg := "payment failed"
		if len(pgResp.Errors) > 0 {
			errMsg = pgResp.Errors[0].Message
		}
		return &CardPaymentResult{
			Status:  PaymentRejected,
			Message: errMsg,
		}, nil
	}

	result := &CardPaymentResult{
		PaymentID:         pgResp.ID,
		Status:            mapPagarmeStatus(pgResp.Status),
		Amount:            int64(pgResp.Amount),
		Installments:      input.Installments,
		ExternalReference: pgResp.Code,
		Message:           getPagarmeStatusMessage(pgResp.Status),
	}

	// Get card info from charge if available
	if len(pgResp.Charges) > 0 {
		charge := pgResp.Charges[0]
		if charge.LastTransaction != nil {
			if card := charge.LastTransaction.Card; card != nil {
				result.LastFourDigits = card.LastFourDigits
				result.CardBrand = card.Brand
			}
			// Pagar.me v5 returns acquirer_auth_code on last_transaction.
			// Fall back to acquirer_nsu when the gateway omits it (some
			// adquirentes only fill one of the two).
			if code := charge.LastTransaction.AcquirerAuthCode; code != "" {
				result.AuthorizationCode = code
			} else if nsu := charge.LastTransaction.AcquirerNsu; nsu != "" {
				result.AuthorizationCode = nsu
			}
		}
		// Pagar.me reports the authorization instant on the charge itself
		// (charges[0].paid_at) — RFC3339 with offset. We prefer this over
		// the server clock so the receipt matches what the customer sees on
		// the gateway dashboard.
		if charge.PaidAt != "" {
			if t, err := time.Parse(time.RFC3339, charge.PaidAt); err == nil {
				result.PaidAt = &t
			}
		}
	}

	return result, nil
}

// GeneratePixPayment generates a PIX QR code for payment.
func (p *Pagarme) GeneratePixPayment(ctx context.Context, input PixPaymentInput) (*PixPaymentResult, error) {
	url := pagarmeAPIBaseURL + "/orders"

	// Set default expiration if not provided
	expiresIn := 30 * time.Minute
	if input.ExpiresIn != nil {
		expiresIn = *input.ExpiresIn
	}
	expiresAt := time.Now().Add(expiresIn)

	// Build items array
	items := make([]map[string]any, len(input.Items))
	for i, item := range input.Items {
		items[i] = map[string]any{
			"amount":      item.UnitPrice,
			"description": item.Name,
			"quantity":    item.Quantity,
			"code":        item.ID,
		}
	}

	// Build customer object
	customer := map[string]any{
		"name":  input.Customer.Name,
		"email": input.Customer.Email,
		"type":  "individual",
	}
	if input.Customer.Document != "" {
		customer["document"] = input.Customer.Document
		customer["document_type"] = "cpf"
	}
	if input.Customer.Phone != "" {
		customer["phones"] = map[string]any{
			"mobile_phone": map[string]any{
				"country_code": "55",
				"area_code":    extractAreaCode(input.Customer.Phone),
				"number":       extractPhoneNumber(input.Customer.Phone),
			},
		}
	}

	// Build PIX payment
	pixPayment := map[string]any{
		"payment_method": "pix",
		"amount":         input.TotalAmount,
		"pix": map[string]any{
			"expires_in": int(expiresIn.Seconds()),
		},
	}

	// Build payload
	payload := map[string]any{
		"code":     input.CartID,
		"items":    items,
		"customer": customer,
		"payments": []map[string]any{pixPayment},
	}

	if input.Metadata != nil {
		payload["metadata"] = input.Metadata
	}

	// Add idempotency key header
	headers := p.authHeaders()
	headers["X-Idempotency-Key"] = fmt.Sprintf("pix-%s-%d", input.CartID, input.TotalAmount)

	resp, body, err := p.DoRequest(ctx, http.MethodPost, url, payload, headers)
	if err != nil {
		return nil, fmt.Errorf("generating pix payment: %w", err)
	}

	var pgResp struct {
		ID       string          `json:"id"`
		Code     string          `json:"code"`
		Amount   int             `json:"amount"`
		Status   string          `json:"status"`
		Charges  []pagarmeCharge `json:"charges"`
		Metadata map[string]any  `json:"metadata"`
		Errors   []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &pgResp); err != nil {
		return nil, fmt.Errorf("parsing pix response: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		errMsg := "pix generation failed"
		if len(pgResp.Errors) > 0 {
			errMsg = pgResp.Errors[0].Message
		}
		return nil, fmt.Errorf(errMsg)
	}

	// Get PIX data from charge
	var qrCode, qrCodeText string
	if len(pgResp.Charges) > 0 && pgResp.Charges[0].LastTransaction != nil {
		if pix := pgResp.Charges[0].LastTransaction.Pix; pix != nil {
			qrCode = pix.QRCodeURL
			qrCodeText = pix.QRCode
			if pix.ExpiresAt != "" {
				if t, err := time.Parse(time.RFC3339, pix.ExpiresAt); err == nil {
					expiresAt = t
				}
			}
		}
	}

	return &PixPaymentResult{
		PaymentID:         pgResp.ID,
		Status:            PaymentPending,
		QRCode:            qrCode,
		QRCodeText:        qrCodeText,
		Amount:            int64(pgResp.Amount),
		ExpiresAt:         expiresAt,
		ExternalReference: pgResp.Code,
	}, nil
}

// GetPaymentMethods returns the available payment methods.
func (p *Pagarme) GetPaymentMethods(ctx context.Context) ([]string, error) {
	// Pagar.me supports both card and pix
	return []string{"card", "pix"}, nil
}

// getPagarmeStatusMessage returns a user-friendly message for a payment status.
func getPagarmeStatusMessage(status string) string {
	switch status {
	case "paid":
		return "Pagamento aprovado"
	case "pending":
		return "Pagamento pendente"
	case "failed":
		return "Pagamento não aprovado"
	case "canceled":
		return "Pagamento cancelado"
	default:
		return "Status: " + status
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
	ID              string                  `json:"id"`
	Code            string                  `json:"code"`
	Amount          int                     `json:"amount"`
	Status          string                  `json:"status"`
	PaymentMethod   string                  `json:"payment_method"`
	PaidAt          string                  `json:"paid_at"`
	CreatedAt       string                  `json:"created_at"`
	UpdatedAt       string                  `json:"updated_at"`
	LastTransaction *pagarmeLastTransaction `json:"last_transaction"`
}

type pagarmeLastTransaction struct {
	ID               string       `json:"id"`
	Status           string       `json:"status"`
	Success          bool         `json:"success"`
	Amount           int          `json:"amount"`
	Installments     int          `json:"installments"`
	Card             *pagarmeCard `json:"card"`
	Pix              *pagarmePix  `json:"pix"`
	AcquirerAuthCode string       `json:"acquirer_auth_code"`
	AcquirerNsu      string       `json:"acquirer_nsu"`
	AcquirerTid      string       `json:"acquirer_tid"`
	CreatedAt        string       `json:"created_at"`
	UpdatedAt        string       `json:"updated_at"`
}

type pagarmeCard struct {
	ID             string `json:"id"`
	FirstSixDigits string `json:"first_six_digits"`
	LastFourDigits string `json:"last_four_digits"`
	Brand          string `json:"brand"`
	HolderName     string `json:"holder_name"`
	ExpMonth       int    `json:"exp_month"`
	ExpYear        int    `json:"exp_year"`
}

type pagarmePix struct {
	QRCode    string `json:"qr_code"`
	QRCodeURL string `json:"qr_code_url"`
	ExpiresAt string `json:"expires_at"`
}

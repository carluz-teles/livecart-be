package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

const (
	mpAPIBaseURL = "https://api.mercadopago.com"
	mpOAuthURL   = "https://api.mercadopago.com/oauth/token"
)

// FlexibleStatus handles Mercado Pago API responses where status can be
// either a string (payment status like "pending") or a number (HTTP error code like 401)
type FlexibleStatus string

func (f *FlexibleStatus) UnmarshalJSON(data []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = FlexibleStatus(s)
		return nil
	}
	// Try number (error responses return HTTP status code)
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*f = FlexibleStatus(fmt.Sprintf("%d", n))
		return nil
	}
	return fmt.Errorf("status field is neither string nor number")
}

// String returns the string value of the status
func (f FlexibleStatus) String() string {
	return string(f)
}

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
		Status            FlexibleStatus `json:"status"`
		StatusDetail      string         `json:"status_detail"`
		TransactionAmount float64        `json:"transaction_amount"`
		CurrencyID        string         `json:"currency_id"`
		DateApproved      string         `json:"date_approved,omitempty"`
		DateCreated       string         `json:"date_created"`
		MoneyReleaseDate  string         `json:"money_release_date,omitempty"`
		Metadata          map[string]any `json:"metadata"`
		ExternalReference string         `json:"external_reference"`
		PaymentTypeID     string         `json:"payment_type_id"`  // credit_card, debit_card, pix, ticket (boleto)
		PaymentMethodID   string         `json:"payment_method_id"` // visa, master, pix, etc.
		Installments      int            `json:"installments"`
	}
	if err := json.Unmarshal(body, &mpPayment); err != nil {
		return nil, fmt.Errorf("parsing payment response: %w", err)
	}

	status := mapMPStatus(mpPayment.Status.String())

	var paidAt *time.Time
	if mpPayment.DateApproved != "" {
		if t, err := time.Parse(time.RFC3339, mpPayment.DateApproved); err == nil {
			paidAt = &t
		}
	}

	var moneyReleaseDate *time.Time
	if mpPayment.MoneyReleaseDate != "" {
		if t, err := time.Parse(time.RFC3339, mpPayment.MoneyReleaseDate); err == nil {
			moneyReleaseDate = &t
		}
	}

	// Map Mercado Pago payment type to our payment method
	paymentMethod := mapMPPaymentType(mpPayment.PaymentTypeID)

	return &PaymentStatus{
		PaymentID:         fmt.Sprintf("%d", mpPayment.ID),
		Status:            status,
		Amount:            int64(mpPayment.TransactionAmount * 100), // Convert to cents
		PaidAt:            paidAt,
		FailureReason:     mpPayment.StatusDetail,
		Metadata:          mpPayment.Metadata,
		ExternalReference: mpPayment.ExternalReference,
		PaymentMethod:     paymentMethod,
		Installments:      mpPayment.Installments,
		MoneyReleaseDate:  moneyReleaseDate,
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
		Status      FlexibleStatus  `json:"status"`
		Amount      float64 `json:"amount"`
		DateCreated string  `json:"date_created"`
	}
	if err := json.Unmarshal(body, &mpRefund); err != nil {
		return nil, fmt.Errorf("parsing refund response: %w", err)
	}

	createdAt, _ := time.Parse(time.RFC3339, mpRefund.DateCreated)

	return &RefundResult{
		RefundID:  fmt.Sprintf("%d", mpRefund.ID),
		Status:    mpRefund.Status.String(),
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

// =============================================================================
// TRANSPARENT CHECKOUT METHODS
// =============================================================================

// GetPublicKey returns the public key for client-side SDK initialization.
func (m *MercadoPago) GetPublicKey(ctx context.Context) (string, error) {
	// Check if we have the public key in Extra credentials
	if m.credentials.Extra != nil {
		if pk, ok := m.credentials.Extra["public_key"].(string); ok && pk != "" {
			return pk, nil
		}
	}

	// Fetch public key from Mercado Pago API
	url := mpAPIBaseURL + "/users/me"
	resp, body, err := m.DoRequest(ctx, http.MethodGet, url, nil, m.authHeaders())
	if err != nil {
		return "", fmt.Errorf("fetching user info: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return "", fmt.Errorf("failed to get user info: status %d", resp.StatusCode)
	}

	var userInfo struct {
		ID   int64 `json:"id"`
		Site string `json:"site_id"`
	}
	if err := json.Unmarshal(body, &userInfo); err != nil {
		return "", fmt.Errorf("parsing user info: %w", err)
	}

	// For Mercado Pago, public key is stored during OAuth or needs to be fetched
	// If not available, we return an error instructing to reconnect the integration
	return "", fmt.Errorf("public key not available. Please reconnect the Mercado Pago integration")
}

// ProcessCardPayment processes a payment with a tokenized card via the
// Orders API v2 (`/v1/orders`). Mercado Pago is migrating new integrations
// off the legacy `/v1/payments` and only `/v1/orders` is documented as
// fully supported in sandbox today.
func (m *MercadoPago) ProcessCardPayment(ctx context.Context, input CardPaymentInput) (*CardPaymentResult, error) {
	// We don't currently distinguish credit vs debit at the input layer —
	// debit cards are routed through Mercado Pago as `debmaster`/`debvisa`
	// payment_method_ids, and the Orders API derives the `type` from that
	// when omitted. Defaulting to credit_card covers >99% of the volume
	// without misclassifying debits because MP's own classification wins.
	paymentMethod := map[string]any{
		"type":         "credit_card",
		"token":        input.Token,
		"installments": input.Installments,
	}
	if input.PaymentMethodID != "" {
		paymentMethod["id"] = input.PaymentMethodID
	}
	// `statement_descriptor` is documented inside payment_method on the older
	// v1/payments examples but the Orders API gateway rejects it as
	// `unprocessable_content` for some seller configurations. Skip until MP
	// publishes a stable schema for it.

	// Orders API's documented minimal payer is just `email`. MP's gateway
	// rejects unknown additional properties, so we keep the buyer info to
	// the bare minimum here. `external_reference` (cart_id) is enough for
	// the BE to recover full customer details if needed.
	payer := map[string]any{"email": input.Customer.Email}

	payload := map[string]any{
		"type":               "online",
		"processing_mode":    "automatic",
		"external_reference": input.CartID,
		// Orders API expects amounts as strings with two decimal places.
		"total_amount": fmt.Sprintf("%.2f", float64(input.TotalAmount)/100),
		"payer":        payer,
		"transactions": map[string]any{
			"payments": []map[string]any{{
				"amount":         fmt.Sprintf("%.2f", float64(input.TotalAmount)/100),
				"payment_method": paymentMethod,
			}},
		},
	}
	// `metadata` and `marketplace` were accepted by /v1/payments but Orders
	// API rejects unknown properties on regular (non-split) charges. The
	// `external_reference` (cart_id) is enough for our webhook handler to
	// look up cart/store/event from our DB.

	// Idempotency key includes UnixNano so each shopper attempt is a fresh
	// request — without it, a previously-failed cart stayed stuck on MP's
	// cached error for ~24h. Retries inside this same call still dedupe
	// because we set the header once per invocation.
	headers := m.authHeaders()
	headers["X-Idempotency-Key"] = fmt.Sprintf(
		"card-%s-%d-%d", input.CartID, input.TotalAmount, time.Now().UnixNano(),
	)
	// Device fingerprint (MP_DEVICE_SESSION_ID from the SDK) belongs in the
	// X-meli-session-id header for anti-fraud, not in additional_info.ip_address.
	if input.DeviceID != "" {
		headers["X-meli-session-id"] = input.DeviceID
	}

	resp, body, err := m.DoRequest(ctx, http.MethodPost, mpAPIBaseURL+"/v1/orders", payload, headers)
	if err != nil {
		return nil, fmt.Errorf("processing card payment: %w", err)
	}

	var orderResp orderResponse
	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("parsing order response: %w", err)
	}

	// On error responses MP wraps payment data inside `data.transactions`.
	// Use that for status_detail when the top-level transactions array is
	// empty (network/validation failures sometimes leave it null).
	payments := orderResp.Transactions.Payments
	if len(payments) == 0 && orderResp.Data != nil {
		payments = orderResp.Data.Transactions.Payments
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		errMsg := firstErrorMessage(&orderResp)
		if errMsg == "" {
			errMsg = fmt.Sprintf("payment failed with status %d", resp.StatusCode)
		}
		safePayload := redactToken(payload)
		statusDetail := ""
		if len(payments) > 0 {
			statusDetail = payments[0].StatusDetail
		}
		m.Logger.Error("mercado pago rejected card payment",
			zap.Int("status_code", resp.StatusCode),
			zap.String("status_detail", statusDetail),
			zap.ByteString("body", body),
			zap.Any("request_payload", safePayload),
		)
		return &CardPaymentResult{
			Status:       PaymentRejected,
			StatusDetail: statusDetail,
			Message:      errMsg,
		}, nil
	}

	if len(payments) == 0 {
		return nil, fmt.Errorf("order response missing transactions.payments")
	}
	pay := payments[0]

	result := &CardPaymentResult{
		PaymentID:         pay.ID,
		Status:            mapMPStatus(pay.Status),
		StatusDetail:      pay.StatusDetail,
		Amount:            input.TotalAmount,
		Installments:      input.Installments,
		CardBrand:         input.PaymentMethodID,
		ExternalReference: orderResp.ExternalReference,
		Message:           getStatusMessage(pay.Status, pay.StatusDetail),
	}
	if pay.LastFourDigits != "" {
		result.LastFourDigits = pay.LastFourDigits
	}
	if pay.AuthorizationCode != "" {
		result.AuthorizationCode = pay.AuthorizationCode
	}
	if pay.DateApproved != "" {
		if t, err := time.Parse(time.RFC3339, pay.DateApproved); err == nil {
			result.PaidAt = &t
		}
	}
	return result, nil
}

// GeneratePixPayment generates a PIX QR code via the Orders API v2.
// In sandbox MP only honours PIX through `/v1/orders`; the legacy
// `/v1/payments` path returns `user_allowed_only_in_test`.
func (m *MercadoPago) GeneratePixPayment(ctx context.Context, input PixPaymentInput) (*PixPaymentResult, error) {
	expiresIn := 30 * time.Minute
	if input.ExpiresIn != nil {
		expiresIn = *input.ExpiresIn
	}
	expiresAt := time.Now().Add(expiresIn)

	payer := buildPayer(input.Customer)
	// In sandbox the seller must opt into MP's PIX simulation by setting
	// `payer.first_name = "APRO"` — without it the QR code is generated but
	// the payment hangs in `action_required` forever. In production this
	// override is harmless: real PIX flow ignores first_name semantics, and
	// the buyer's actual name is captured separately via the bank app.
	if strings.HasPrefix(m.credentials.AccessToken, "TEST-") {
		payer["first_name"] = "APRO"
	}

	payload := map[string]any{
		"type":               "online",
		"processing_mode":    "automatic",
		"external_reference": input.CartID,
		"total_amount":       fmt.Sprintf("%.2f", float64(input.TotalAmount)/100),
		"payer":              payer,
		"transactions": map[string]any{
			"payments": []map[string]any{{
				"amount": fmt.Sprintf("%.2f", float64(input.TotalAmount)/100),
				"payment_method": map[string]any{
					"id":   "pix",
					"type": "bank_transfer",
				},
				// `expiration_time` is ISO 8601 duration, not a timestamp.
				"expiration_time": durationToISO8601(expiresIn),
			}},
		},
	}
	// `metadata` not allowed by Orders API — `external_reference` (cart_id)
	// is sufficient to recover cart/store/event from our DB on webhook.

	headers := m.authHeaders()
	headers["X-Idempotency-Key"] = fmt.Sprintf(
		"pix-%s-%d-%d", input.CartID, input.TotalAmount, time.Now().UnixNano(),
	)

	resp, body, err := m.DoRequest(ctx, http.MethodPost, mpAPIBaseURL+"/v1/orders", payload, headers)
	if err != nil {
		return nil, fmt.Errorf("generating pix payment: %w", err)
	}

	var orderResp orderResponse
	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("parsing pix order response: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		errMsg := firstErrorMessage(&orderResp)
		if errMsg == "" {
			errMsg = fmt.Sprintf("pix generation failed with status %d", resp.StatusCode)
		}
		m.Logger.Error("mercado pago rejected pix order",
			zap.Int("status_code", resp.StatusCode),
			zap.ByteString("body", body),
			zap.Any("request_payload", payload),
		)
		return nil, fmt.Errorf("pix generation failed: %s", errMsg)
	}

	if len(orderResp.Transactions.Payments) == 0 {
		return nil, fmt.Errorf("pix order response missing transactions.payments")
	}
	pay := orderResp.Transactions.Payments[0]

	return &PixPaymentResult{
		PaymentID:         pay.ID,
		Status:            PaymentPending,
		QRCode:            pay.PaymentMethod.QRCodeBase64,
		QRCodeText:        pay.PaymentMethod.QRCode,
		Amount:            input.TotalAmount,
		ExpiresAt:         expiresAt,
		ExternalReference: orderResp.ExternalReference,
		TicketURL:         pay.PaymentMethod.TicketURL,
	}, nil
}

// orderResponse is the shape of /v1/orders responses we care about.
// Both success and failure responses include a `transactions.payments`
// array; on failure the same shape is repeated under `data` and an
// `errors` array carries the human messages.
type orderResponse struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	StatusDetail      string `json:"status_detail"`
	ExternalReference string `json:"external_reference"`
	Transactions      struct {
		Payments []orderPayment `json:"payments"`
	} `json:"transactions"`
	Data *struct {
		Transactions struct {
			Payments []orderPayment `json:"payments"`
		} `json:"transactions"`
	} `json:"data,omitempty"`
	Errors []orderError `json:"errors,omitempty"`
	// Older error envelope (legacy `/v1/payments` returned these top-level
	// fields). Some MP error paths still echo them on /v1/orders.
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type orderError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

type orderPayment struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	StatusDetail      string             `json:"status_detail"`
	Amount            string             `json:"amount"`
	ReferenceID       string             `json:"reference_id"`
	DateApproved      string             `json:"date_approved"`
	LastFourDigits    string             `json:"last_four_digits"`
	AuthorizationCode string             `json:"authorization_code"`
	PaymentMethod     orderPaymentMethod `json:"payment_method"`
}

type orderPaymentMethod struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	QRCode       string `json:"qr_code"`
	QRCodeBase64 string `json:"qr_code_base64"`
	TicketURL    string `json:"ticket_url"`
}

func firstErrorMessage(r *orderResponse) string {
	if len(r.Errors) > 0 {
		if r.Errors[0].Message != "" {
			return r.Errors[0].Message
		}
		if len(r.Errors[0].Details) > 0 {
			return r.Errors[0].Details[0]
		}
	}
	if r.Message != "" {
		return r.Message
	}
	return r.Error
}

func buildPayer(c CheckoutCustomer) map[string]any {
	payer := map[string]any{"email": c.Email}
	if c.Document != "" {
		payer["identification"] = map[string]string{
			"type":   "CPF",
			"number": c.Document,
		}
	}
	if c.Name != "" {
		names := splitName(c.Name)
		payer["first_name"] = names[0]
		if len(names) > 1 {
			payer["last_name"] = names[1]
		}
	}
	return payer
}

func redactToken(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if k == "transactions" {
			out[k] = redactTokenInTransactions(v)
			continue
		}
		out[k] = v
	}
	return out
}

func redactTokenInTransactions(v any) any {
	tx, ok := v.(map[string]any)
	if !ok {
		return v
	}
	payments, ok := tx["payments"].([]map[string]any)
	if !ok {
		return v
	}
	clonedPayments := make([]map[string]any, len(payments))
	for i, p := range payments {
		cp := make(map[string]any, len(p))
		for k, val := range p {
			if k == "payment_method" {
				if pm, ok := val.(map[string]any); ok {
					cm := make(map[string]any, len(pm))
					for pk, pv := range pm {
						if pk == "token" {
							cm[pk] = "[redacted]"
						} else {
							cm[pk] = pv
						}
					}
					cp[k] = cm
					continue
				}
			}
			cp[k] = val
		}
		clonedPayments[i] = cp
	}
	clone := make(map[string]any, len(tx))
	for k, val := range tx {
		clone[k] = val
	}
	clone["payments"] = clonedPayments
	return clone
}

// durationToISO8601 turns a time.Duration into an ISO 8601 duration string
// like "PT30M". Orders API expects this for `expiration_time`.
func durationToISO8601(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	totalSec := int64(d.Seconds())
	hours := totalSec / 3600
	minutes := (totalSec % 3600) / 60
	seconds := totalSec % 60
	out := "PT"
	if hours > 0 {
		out += fmt.Sprintf("%dH", hours)
	}
	if minutes > 0 {
		out += fmt.Sprintf("%dM", minutes)
	}
	if seconds > 0 || (hours == 0 && minutes == 0) {
		out += fmt.Sprintf("%dS", seconds)
	}
	return out
}

// GetPaymentMethods returns the payment methods actually enabled on the
// collector's Mercado Pago account. Queries /v1/payment_methods and keeps
// only entries whose status is active. PIX is filtered out when the account
// hasn't registered a PIX key (or deactivated QR rendering), so the frontend
// never offers an option the merchant cannot fulfill.
//
// On any failure talking to Mercado Pago we fall back to ["card"] to keep
// the checkout working — we only offer PIX when we have positive evidence
// that it is enabled.
func (m *MercadoPago) GetPaymentMethods(ctx context.Context) ([]string, error) {
	url := mpAPIBaseURL + "/v1/payment_methods"
	resp, body, err := m.DoRequest(ctx, http.MethodGet, url, nil, m.authHeaders())
	if err != nil || !providers.IsSuccessStatus(resp.StatusCode) {
		m.Logger.Warn("mercado pago payment_methods lookup failed, falling back to card-only",
			zap.Int("status", statusOf(resp)),
			zap.Error(err),
		)
		return []string{"card"}, nil
	}

	var entries []struct {
		ID              string `json:"id"`
		PaymentTypeID   string `json:"payment_type_id"`
		Status          string `json:"status"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		m.Logger.Warn("mercado pago payment_methods parse failed, falling back to card-only",
			zap.Error(err),
		)
		return []string{"card"}, nil
	}

	hasCard, hasPIX := false, false
	for _, e := range entries {
		if e.Status != "active" {
			continue
		}
		switch e.PaymentTypeID {
		case "credit_card", "debit_card":
			hasCard = true
		case "bank_transfer":
			if e.ID == "pix" {
				hasPIX = true
			}
		}
	}

	methods := make([]string, 0, 2)
	if hasCard {
		methods = append(methods, "card")
	}
	if hasPIX {
		methods = append(methods, "pix")
	}
	if len(methods) == 0 {
		methods = append(methods, "card") // last-resort fallback
	}
	return methods, nil
}

// statusOf returns the HTTP status when the response is not nil.
func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// mapMPPaymentType maps Mercado Pago payment_type_id to our payment method.
func mapMPPaymentType(paymentType string) string {
	switch paymentType {
	case "credit_card":
		return "credit_card"
	case "debit_card":
		return "debit_card"
	case "pix", "bank_transfer":
		return "pix"
	case "ticket":
		return "boleto"
	default:
		return "other"
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

// splitName splits a full name into first and last name.
func splitName(fullName string) []string {
	parts := make([]string, 0, 2)
	current := ""
	for _, r := range fullName {
		if r == ' ' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(r)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	if len(parts) == 0 {
		return []string{fullName}
	}
	if len(parts) == 1 {
		return parts
	}
	// Return first name and the rest joined as last name
	return []string{parts[0], joinStrings(parts[1:], " ")}
}

// joinStrings joins strings with a separator.
func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}

// getStatusMessage returns a user-friendly message for a payment status.
func getStatusMessage(status, detail string) string {
	switch status {
	case "approved":
		return "Pagamento aprovado"
	case "pending":
		return "Pagamento pendente de confirmação"
	case "in_process":
		return "Pagamento em processamento"
	case "rejected":
		return getRejectMessage(detail)
	default:
		return "Status do pagamento: " + status
	}
}

// getRejectMessage returns a user-friendly message for rejection reasons.
func getRejectMessage(detail string) string {
	switch detail {
	case "cc_rejected_insufficient_amount":
		return "Saldo insuficiente"
	case "cc_rejected_bad_filled_security_code":
		return "Código de segurança inválido"
	case "cc_rejected_bad_filled_date":
		return "Data de validade inválida"
	case "cc_rejected_bad_filled_other":
		return "Dados do cartão incorretos"
	case "cc_rejected_call_for_authorize":
		return "Entre em contato com a operadora do cartão"
	case "cc_rejected_card_disabled":
		return "Cartão desabilitado"
	case "cc_rejected_duplicated_payment":
		return "Pagamento duplicado"
	case "cc_rejected_high_risk":
		return "Pagamento rejeitado por segurança"
	case "cc_rejected_max_attempts":
		return "Limite de tentativas excedido"
	default:
		return "Pagamento não aprovado"
	}
}

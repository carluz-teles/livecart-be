package payment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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

// ValidateCredentials validates the current credentials by hitting
// /recipients?size=1 — a paginated listing that authenticates the key
// without requiring any business setup. The previous probe used
// /recipients/default, which returns 412 on accounts that don't have a
// "default recipient" configured (anything that isn't a marketplace
// split-payment setup), so brand-new Pagar.me accounts couldn't connect
// even with a perfectly valid sk_test_/sk_live_ key.
func (p *Pagarme) ValidateCredentials(ctx context.Context) error {
	url := pagarmeAPIBaseURL + "/recipients?size=1"

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

// TestConnection tests the connection to Pagar.me API and surfaces account
// info to populate the "Informações da Conta" card in the admin UI. Uses
// /recipients?size=1 for authentication (works on any active account) and,
// when a recipient is returned, uses it for the displayed name/email/doc.
// Accounts without recipients yet still pass — the card just shows the
// environment (sandbox/production) from the key prefix.
func (p *Pagarme) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	url := pagarmeAPIBaseURL + "/recipients?size=1"

	resp, body, err := p.DoRequest(ctx, http.MethodGet, url, nil, p.authHeaders())
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
		result.Message = "Chave de API inválida"
		return result, nil
	}

	if resp.StatusCode != http.StatusOK {
		result.Success = false
		result.Message = fmt.Sprintf("Erro na API: status %d", resp.StatusCode)
		return result, nil
	}

	var listing struct {
		Data []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Email    string `json:"email"`
			Document string `json:"document"`
			Type     string `json:"type"`
			Status   string `json:"status"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &listing) // best-effort; missing fields stay empty

	accountInfo := map[string]any{
		"environment": pagarmeEnvironmentFromKey(p.credentials.APIKey),
	}
	if len(listing.Data) > 0 {
		r := listing.Data[0]
		accountInfo["id"] = r.ID
		accountInfo["name"] = r.Name
		accountInfo["email"] = r.Email
		accountInfo["document"] = r.Document
	}

	result.Success = true
	result.Message = "Conexão estabelecida com sucesso"
	result.AccountInfo = accountInfo
	return result, nil
}

// pagarmeEnvironmentFromKey returns "sandbox" / "production" / "unknown"
// based on the secret key prefix. Pagar.me has no separate sandbox host;
// the test/live segment in the key is the only switch.
func pagarmeEnvironmentFromKey(secretKey string) string {
	switch {
	case strings.HasPrefix(secretKey, "sk_test_"):
		return "sandbox"
	case strings.HasPrefix(secretKey, "sk_live_"):
		return "production"
	}
	return "unknown"
}

// RefreshToken refreshes OAuth tokens - Pagar.me uses API keys, so no refresh needed.
func (p *Pagarme) RefreshToken(ctx context.Context) (*Credentials, error) {
	// Pagar.me uses API keys, not OAuth tokens, so no refresh is needed
	return nil, nil
}

// CreateCheckout creates a hosted-checkout order at Pagar.me. Live commerce
// only takes credit_card + pix today (boleto's 1-3 day clearance kills the
// "live now" UX), so the accepted_payment_methods list is intentionally narrow.
func (p *Pagarme) CreateCheckout(ctx context.Context, order CheckoutOrder) (*CheckoutResult, error) {
	url := pagarmeAPIBaseURL + "/orders"

	items := make([]map[string]any, len(order.Items))
	for i, item := range order.Items {
		items[i] = map[string]any{
			"amount":      item.UnitPrice,
			"description": item.Name,
			"quantity":    item.Quantity,
			"code":        item.ID,
		}
	}

	customer := buildPagarmeCustomer(order.Customer)

	expiresInMinutes := 120
	if order.ExpiresIn != nil {
		expiresInMinutes = int(order.ExpiresIn.Minutes())
	}

	checkoutConfig := map[string]any{
		"expires_in":               expiresInMinutes,
		"billing_address_editable": true,
		"customer_editable":        true,
		"accepted_payment_methods": []string{"credit_card", "pix"},
		"success_url":              order.SuccessURL,
		"credit_card": map[string]any{
			"capture":              true,
			"statement_descriptor": "LIVECART",
			"installments": []map[string]any{
				{"number": 1, "total": order.TotalAmount},
			},
		},
		"pix": map[string]any{
			"expires_in": expiresInMinutes * 60,
		},
	}

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

// GetPaymentStatus retrieves the status of a Pagar.me order. The fee/net
// breakdown stays empty here — Pagar.me v5 surfaces fees on /recipients/balance
// (separate API), not on the order. The audit log mirrors the MP shape so
// downstream observability dashboards (status, payment_method, installments,
// auth_code) work uniformly across providers.
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

	var (
		paidAt        *time.Time
		paymentMethod string
		statusDetail  string
		installments  int
		authCode      string
	)
	if len(pgOrder.Charges) > 0 {
		charge := pgOrder.Charges[0]
		if charge.PaidAt != "" {
			if t, err := time.Parse(time.RFC3339, charge.PaidAt); err == nil {
				paidAt = &t
			}
		}
		paymentMethod = mapPagarmePaymentMethod(charge.PaymentMethod)
		if charge.LastTransaction != nil {
			installments = charge.LastTransaction.Installments
			statusDetail = charge.LastTransaction.Status
			if code := charge.LastTransaction.AcquirerAuthCode; code != "" {
				authCode = code
			} else if nsu := charge.LastTransaction.AcquirerNsu; nsu != "" {
				authCode = nsu
			}
		}
	}

	p.Logger.Info("pagarme order status fetched",
		zap.String("payment_id", pgOrder.ID),
		zap.String("status", string(status)),
		zap.String("status_detail", statusDetail),
		zap.String("payment_method", paymentMethod),
		zap.Int("installments", installments),
		zap.Int64("amount_cents", int64(pgOrder.Amount)),
		zap.String("authorization_code", authCode),
		zap.Bool("has_paid_at", paidAt != nil),
	)

	return &PaymentStatus{
		PaymentID:         pgOrder.ID,
		Status:            status,
		Amount:            int64(pgOrder.Amount),
		PaidAt:            paidAt,
		FailureReason:     statusDetail,
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

// stripPhonePrefix returns the BR-relevant digits of a phone string —
// non-digits removed, "55" country prefix dropped when present (13-digit
// inputs like "5547992596026" reduce to the 11-digit "47992596026").
func stripPhonePrefix(phone string) string {
	cleaned := digitsOnly(phone)
	if len(cleaned) == 13 && strings.HasPrefix(cleaned, "55") {
		return cleaned[2:]
	}
	return cleaned
}

func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// extractAreaCode pulls the 2-digit DDD from a Brazilian phone string.
// Defaults to "11" (São Paulo) only when the input is too short to
// contain one — Pagar.me requires the field non-empty.
func extractAreaCode(phone string) string {
	cleaned := stripPhonePrefix(phone)
	if len(cleaned) >= 2 {
		return cleaned[:2]
	}
	return "11"
}

// extractPhoneNumber returns the local-part digits (everything after the DDD).
func extractPhoneNumber(phone string) string {
	cleaned := stripPhonePrefix(phone)
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

// ProcessCardPayment processes a payment with a tokenized card via the
// Pagar.me v5 /orders endpoint. The X-Idempotency-Key is a fresh UUID per
// call (not derived from cart+amount) so a retry after a rejected attempt
// gets evaluated as a new charge instead of replaying the cached failure.
func (p *Pagarme) ProcessCardPayment(ctx context.Context, input CardPaymentInput) (*CardPaymentResult, error) {
	url := pagarmeAPIBaseURL + "/orders"

	items := make([]map[string]any, len(input.Items))
	for i, item := range input.Items {
		items[i] = map[string]any{
			"amount":      item.UnitPrice,
			"description": item.Name,
			"quantity":    item.Quantity,
			"code":        item.ID,
		}
	}

	customer := buildPagarmeCustomer(input.Customer)

	// Pagar.me v5 mixes `card_token` and `card` mutually: when you set
	// card_token the gateway pulls the card data (including holder_document
	// and billing_address) out of the token itself, set on the FE during
	// the /tokens POST. Sending a partial `card: { holder_document }`
	// alongside the token flips the validator into "raw card mode" and it
	// rejects the order with `validation_error | billing | "value" is
	// required` because the rest of the card object (number, cvv, exp,
	// billing_address) is missing. The FE already includes holder_document
	// inside the token payload, and customer.document covers the buyer-
	// level CPF for the order — there's no need to repeat it on `card`.
	cardConfig := map[string]any{
		"card_token":           input.Token,
		"installments":         input.Installments,
		"statement_descriptor": "LIVECART",
		"capture":              true,
	}

	cardPayment := map[string]any{
		"payment_method": "credit_card",
		"credit_card":    cardConfig,
	}

	payload := map[string]any{
		"code":     input.CartID,
		"items":    items,
		"customer": customer,
		"payments": []map[string]any{cardPayment},
	}

	if input.Metadata != nil {
		payload["metadata"] = input.Metadata
	}

	headers := p.authHeaders()
	headers["X-Idempotency-Key"] = uuid.New().String()

	resp, body, err := p.DoRequest(ctx, http.MethodPost, url, payload, headers)
	if err != nil {
		return nil, fmt.Errorf("processing card payment: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		errMsg := parsePagarmeError(body)
		safe := redactCardPayload(payload)
		p.Logger.Error("pagarme card payment failed",
			zap.Int("status_code", resp.StatusCode),
			zap.String("body", string(body)),
			zap.Any("request_payload", safe),
		)
		// 5xx (and 429) means Pagar.me itself is unhappy — the buyer's card
		// was not charged. Bubble it up as a transport error so the checkout
		// layer returns a retryable 422 instead of telling the buyer "card
		// rejected" when the gateway never actually evaluated the card.
		// Genuine card declines come back as 2xx with status="failed" on the
		// charge and are handled below.
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			return nil, fmt.Errorf("pagar.me indisponível (HTTP %d): %s", resp.StatusCode, errMsg)
		}
		return &CardPaymentResult{
			Status:  PaymentRejected,
			Message: errMsg,
		}, nil
	}

	var pgResp struct {
		ID       string          `json:"id"`
		Code     string          `json:"code"`
		Amount   int             `json:"amount"`
		Status   string          `json:"status"`
		Charges  []pagarmeCharge `json:"charges"`
		Metadata map[string]any  `json:"metadata"`
	}
	if err := json.Unmarshal(body, &pgResp); err != nil {
		return nil, fmt.Errorf("parsing payment response: %w", err)
	}

	result := &CardPaymentResult{
		PaymentID:         pgResp.ID,
		Status:            mapPagarmeStatus(pgResp.Status),
		Amount:            int64(pgResp.Amount),
		Installments:      input.Installments,
		ExternalReference: pgResp.Code,
		Message:           getPagarmeStatusMessage(pgResp.Status),
	}

	var (
		statusDetail       string
		acquirerName       string
		acquirerMessage    string
		acquirerReturnCode string
		gatewayResp        map[string]any
		antifraudResp      map[string]any
	)
	if len(pgResp.Charges) > 0 {
		charge := pgResp.Charges[0]
		if charge.LastTransaction != nil {
			lt := charge.LastTransaction
			statusDetail = lt.Status
			acquirerName = lt.AcquirerName
			acquirerMessage = lt.AcquirerMessage
			acquirerReturnCode = lt.AcquirerReturnCode
			gatewayResp = lt.GatewayResponse
			antifraudResp = lt.AntifraudResponse
			if card := lt.Card; card != nil {
				result.LastFourDigits = card.LastFourDigits
				result.CardBrand = card.Brand
			}
			// Pagar.me v5 returns acquirer_auth_code on last_transaction.
			// Fall back to acquirer_nsu when the gateway omits it (some
			// adquirentes only fill one of the two).
			if code := lt.AcquirerAuthCode; code != "" {
				result.AuthorizationCode = code
			} else if nsu := lt.AcquirerNsu; nsu != "" {
				result.AuthorizationCode = nsu
			}
			// Surface acquirer_message in the buyer-facing copy when the
			// transaction failed — without this, every rejection shows the
			// generic "pagamento recusado" without saying whether it was
			// CVV wrong, no funds, fraud rules, etc.
			if !lt.Success && acquirerMessage != "" {
				result.Message = acquirerMessage
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
	result.StatusDetail = statusDetail

	logFields := []zap.Field{
		zap.String("payment_id", result.PaymentID),
		zap.String("cart_id", input.CartID),
		zap.String("status", string(result.Status)),
		zap.String("status_detail", statusDetail),
		zap.String("payment_method_id", "credit_card"),
		zap.String("card_brand", result.CardBrand),
		zap.Int("installments", result.Installments),
		zap.Int64("amount_cents", result.Amount),
		zap.String("authorization_code", result.AuthorizationCode),
		zap.Bool("has_paid_at", result.PaidAt != nil),
		zap.String("acquirer_name", acquirerName),
		zap.String("acquirer_message", acquirerMessage),
		zap.String("acquirer_return_code", acquirerReturnCode),
		zap.Any("gateway_response", gatewayResp),
		zap.Any("antifraud_response", antifraudResp),
	}
	// Log at Error level on rejection so the message + return code
	// surface in error dashboards alongside the 5xx transport failures.
	// Approved/pending payments stay at Info. On rejection also dump the
	// (redacted) request payload + the raw body so we can correlate the
	// dashboard's validation_error path against what we actually sent —
	// the captured path is hit even when the gateway's gateway_response
	// is HTTP 400 wrapped inside a 200, so this is the only place that
	// surfaces the request body for that case.
	if result.Status == PaymentRejected {
		safe := redactCardPayload(payload)
		logFields = append(logFields,
			zap.Any("request_payload", safe),
			zap.String("response_body", string(body)),
		)
		p.Logger.Error("pagarme card payment captured", logFields...)
	} else {
		p.Logger.Info("pagarme card payment captured", logFields...)
	}

	return result, nil
}

// GeneratePixPayment generates a PIX QR code via Pagar.me v5 /orders.
// Pagar.me requires customer.phones for PIX, so we reject upfront when
// the cart has no phone instead of letting the gateway return an opaque
// 422. The X-Idempotency-Key is a fresh UUID per call so a retry after
// expiration issues a new QR instead of replaying the dead one.
func (p *Pagarme) GeneratePixPayment(ctx context.Context, input PixPaymentInput) (*PixPaymentResult, error) {
	if strings.TrimSpace(input.Customer.Phone) == "" {
		return nil, fmt.Errorf("telefone do comprador é obrigatório para pagamento PIX")
	}

	url := pagarmeAPIBaseURL + "/orders"

	expiresIn := 30 * time.Minute
	if input.ExpiresIn != nil {
		expiresIn = *input.ExpiresIn
	}
	expiresAt := time.Now().Add(expiresIn)

	items := make([]map[string]any, len(input.Items))
	for i, item := range input.Items {
		items[i] = map[string]any{
			"amount":      item.UnitPrice,
			"description": item.Name,
			"quantity":    item.Quantity,
			"code":        item.ID,
		}
	}

	customer := buildPagarmeCustomer(input.Customer)

	pixPayment := map[string]any{
		"payment_method": "pix",
		"amount":         input.TotalAmount,
		"pix": map[string]any{
			"expires_in": int(expiresIn.Seconds()),
		},
	}

	payload := map[string]any{
		"code":     input.CartID,
		"items":    items,
		"customer": customer,
		"payments": []map[string]any{pixPayment},
	}

	if input.Metadata != nil {
		payload["metadata"] = input.Metadata
	}

	headers := p.authHeaders()
	headers["X-Idempotency-Key"] = uuid.New().String()

	resp, body, err := p.DoRequest(ctx, http.MethodPost, url, payload, headers)
	if err != nil {
		return nil, fmt.Errorf("generating pix payment: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		errMsg := parsePagarmeError(body)
		p.Logger.Error("pagarme rejected pix generation",
			zap.Int("status_code", resp.StatusCode),
			zap.String("body", string(body)),
			zap.String("cart_id", input.CartID),
			zap.Int64("amount_cents", input.TotalAmount),
		)
		return nil, fmt.Errorf("%s", errMsg)
	}

	var pgResp struct {
		ID       string          `json:"id"`
		Code     string          `json:"code"`
		Amount   int             `json:"amount"`
		Status   string          `json:"status"`
		Charges  []pagarmeCharge `json:"charges"`
		Metadata map[string]any  `json:"metadata"`
	}
	if err := json.Unmarshal(body, &pgResp); err != nil {
		return nil, fmt.Errorf("parsing pix response: %w", err)
	}

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

	p.Logger.Info("pagarme pix generated",
		zap.String("payment_id", pgResp.ID),
		zap.String("cart_id", input.CartID),
		zap.Int64("amount_cents", int64(pgResp.Amount)),
		zap.String("expires_at", expiresAt.Format(time.RFC3339)),
	)

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

// buildPagarmeCustomer maps our internal CheckoutCustomer onto the
// Pagar.me v5 customer payload shared by Card / PIX / Checkout flows.
func buildPagarmeCustomer(c CheckoutCustomer) map[string]any {
	customer := map[string]any{
		"name":  c.Name,
		"email": c.Email,
		"type":  "individual",
	}
	if c.Document != "" {
		customer["document"] = c.Document
		customer["document_type"] = "cpf"
	}
	if c.Phone != "" {
		customer["phones"] = map[string]any{
			"mobile_phone": map[string]any{
				"country_code": "55",
				"area_code":    extractAreaCode(c.Phone),
				"number":       extractPhoneNumber(c.Phone),
			},
		}
	}
	if c.Address != nil {
		address := map[string]any{
			"country":      "BR",
			"zip_code":     c.Address.ZipCode,
			"line_1":       buildPagarmeLine1(c.Address.Street, c.Address.Number, c.Address.Neighborhood),
			"city":         c.Address.City,
			"state":        c.Address.State,
		}
		if c.Address.Complement != "" {
			address["line_2"] = c.Address.Complement
		}
		customer["address"] = address
	}
	return customer
}

// buildPagarmeLine1 assembles the Pagar.me line_1 field (free-form street
// address). Pagar.me v5 does not have separate street_name / street_number
// fields like MP — it expects "Rua X, 123, Bairro Y" in a single string.
func buildPagarmeLine1(street, number, neighborhood string) string {
	parts := make([]string, 0, 3)
	if street != "" {
		parts = append(parts, street)
	}
	if number != "" {
		parts = append(parts, number)
	}
	if neighborhood != "" {
		parts = append(parts, neighborhood)
	}
	return strings.Join(parts, ", ")
}

// parsePagarmeError extracts a user-friendly message out of a Pagar.me v5
// error response. The shape is `{ "message": "...", "errors": { "field":
// ["msg1", "msg2"], ... } }` — we surface the first field-level message
// when present (more actionable than the generic top-level `message`),
// falling back to `message` and finally to the raw body.
func parsePagarmeError(body []byte) string {
	var parsed struct {
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		for field, msgs := range parsed.Errors {
			if len(msgs) > 0 {
				return fmt.Sprintf("%s: %s", field, msgs[0])
			}
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	if len(body) > 0 {
		return string(body)
	}
	return "pagamento rejeitado pela Pagar.me"
}

// redactCardPayload strips the card token from a payload before logging.
// Pagar.me's card_token is single-use but leaking it in logs still risks
// it being replayed before the gateway invalidates it.
func redactCardPayload(payload map[string]any) map[string]any {
	clone := make(map[string]any, len(payload))
	for k, v := range payload {
		clone[k] = v
	}
	if payments, ok := clone["payments"].([]map[string]any); ok && len(payments) > 0 {
		safePayments := make([]map[string]any, 0, len(payments))
		for _, pmt := range payments {
			cp := make(map[string]any, len(pmt))
			for k, v := range pmt {
				cp[k] = v
			}
			if cc, ok := cp["credit_card"].(map[string]any); ok {
				ccc := make(map[string]any, len(cc))
				for k, v := range cc {
					ccc[k] = v
				}
				if _, ok := ccc["card_token"]; ok {
					ccc["card_token"] = "[redacted]"
				}
				cp["credit_card"] = ccc
			}
			safePayments = append(safePayments, cp)
		}
		clone["payments"] = safePayments
	}
	return clone
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
	ID                 string                 `json:"id"`
	Status             string                 `json:"status"`
	Success            bool                   `json:"success"`
	Amount             int                    `json:"amount"`
	Installments       int                    `json:"installments"`
	Card               *pagarmeCard           `json:"card"`
	Pix                *pagarmePix            `json:"pix"`
	AcquirerName       string                 `json:"acquirer_name"`
	AcquirerAuthCode   string                 `json:"acquirer_auth_code"`
	AcquirerNsu        string                 `json:"acquirer_nsu"`
	AcquirerTid        string                 `json:"acquirer_tid"`
	AcquirerMessage    string                 `json:"acquirer_message"`
	AcquirerReturnCode string                 `json:"acquirer_return_code"`
	GatewayResponse    map[string]any         `json:"gateway_response"`
	AntifraudResponse  map[string]any         `json:"antifraud_response"`
	CreatedAt          string                 `json:"created_at"`
	UpdatedAt          string                 `json:"updated_at"`
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

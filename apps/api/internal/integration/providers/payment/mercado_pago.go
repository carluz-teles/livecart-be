package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mercadopago/sdk-go/pkg/config"
	"github.com/mercadopago/sdk-go/pkg/mperror"
	"github.com/mercadopago/sdk-go/pkg/oauth"
	"github.com/mercadopago/sdk-go/pkg/payment"
	"github.com/mercadopago/sdk-go/pkg/paymentmethod"
	"github.com/mercadopago/sdk-go/pkg/preference"
	"github.com/mercadopago/sdk-go/pkg/refund"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

// MP item category — kept generic for now. Live commerce on the platform
// is dominantly fashion but we'd rather not bias the fraud-screening signal
// away from non-fashion stores. Will become per-store config later.
const mpDefaultCategoryID = "others"

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

// RefreshToken refreshes the OAuth access token via the sdk-go OAuth client.
// The SDK init takes the *app secret* as its "access token" — the OAuth
// endpoints use it as client_secret in the request body. Our long-lived
// merchant access token (m.credentials.AccessToken) is unrelated here.
func (m *MercadoPago) RefreshToken(ctx context.Context) (*Credentials, error) {
	if m.credentials.RefreshToken == "" {
		return nil, nil // No refresh token available
	}
	if m.appSecret == "" {
		return nil, fmt.Errorf("app credentials required for token refresh")
	}

	cfg, err := config.New(m.appSecret)
	if err != nil {
		return nil, fmt.Errorf("init mp oauth config: %w", err)
	}

	resource, err := oauth.NewClient(cfg).Refresh(ctx, m.credentials.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("refreshing token: %w", err)
	}

	return &Credentials{
		AccessToken:  resource.AccessToken,
		RefreshToken: resource.RefreshToken,
		TokenType:    resource.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(resource.ExpiresIn) * time.Second),
	}, nil
}

// CreateCheckout creates a checkout preference in Mercado Pago via the
// official sdk-go. Beyond the migration, this fills three previously-missing
// fields the MP integration scoring report flagged: items[].category_id,
// payer.first_name/last_name (split out of the full name we capture as a
// single field), and payer.address (when the buyer has filled it — Checkout
// Pro flows hand off the form to the gateway, so address won't be present).
func (m *MercadoPago) CreateCheckout(ctx context.Context, order CheckoutOrder) (*CheckoutResult, error) {
	cfg, err := m.sdkConfig()
	if err != nil {
		return nil, err
	}

	items := make([]preference.ItemRequest, len(order.Items))
	for i, item := range order.Items {
		items[i] = preference.ItemRequest{
			ID:          item.ID,
			Title:       item.Name,
			Description: item.Description,
			PictureURL:  item.ImageURL,
			CategoryID:  mpDefaultCategoryID,
			CurrencyID:  order.Currency,
			UnitPrice:   float64(item.UnitPrice) / 100, // cents → currency units
			Quantity:    item.Quantity,
		}
	}

	payer := &preference.PayerRequest{
		Email: order.Customer.Email,
	}
	// MP wants name + surname split. We capture a single Name field, so
	// fall back to the full name as Name when we can't infer a surname.
	if order.Customer.Name != "" {
		parts := splitName(order.Customer.Name)
		payer.Name = parts[0]
		if len(parts) > 1 {
			payer.Surname = parts[1]
		}
	}
	if order.Customer.Phone != "" {
		payer.Phone = &preference.PhoneRequest{Number: order.Customer.Phone}
	}
	if order.Customer.Document != "" {
		payer.Identification = &preference.IdentificationRequest{
			Type:   "CPF",
			Number: order.Customer.Document,
		}
	}
	if order.Customer.Address != nil {
		payer.Address = &preference.AddressRequest{
			ZipCode:      order.Customer.Address.ZipCode,
			StreetName:   order.Customer.Address.Street,
			StreetNumber: order.Customer.Address.Number,
		}
	}

	req := preference.Request{
		Items: items,
		Payer: payer,
		BackURLs: &preference.BackURLsRequest{
			Success: order.SuccessURL,
			Failure: order.FailureURL,
			Pending: order.SuccessURL,
		},
		AutoReturn:        "approved",
		ExternalReference: order.ExternalID,
		NotificationURL:   order.NotifyURL,
		Metadata:          order.Metadata,
	}

	if order.ExpiresIn != nil {
		expiration := time.Now().Add(*order.ExpiresIn)
		req.ExpirationDateTo = &expiration
		req.Expires = true
	}

	resource, err := preference.NewClient(cfg).Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("creating checkout: %w", err)
	}

	// Sandbox URL falls back when init_point is empty (test credentials).
	checkoutURL := resource.InitPoint
	if checkoutURL == "" {
		checkoutURL = resource.SandboxInitPoint
	}

	var expiresAt *time.Time
	if !resource.DateOfExpiration.IsZero() {
		t := resource.DateOfExpiration
		expiresAt = &t
	}

	return &CheckoutResult{
		CheckoutID:  resource.ID,
		CheckoutURL: checkoutURL,
		ExpiresAt:   expiresAt,
	}, nil
}

// GetPaymentStatus retrieves the status of a payment via the sdk-go
// Payment client.
func (m *MercadoPago) GetPaymentStatus(ctx context.Context, paymentID string) (*PaymentStatus, error) {
	id, err := strconv.Atoi(paymentID)
	if err != nil {
		return nil, fmt.Errorf("invalid payment id %q: %w", paymentID, err)
	}

	cfg, err := m.sdkConfig()
	if err != nil {
		return nil, err
	}

	resource, err := payment.NewClient(cfg).Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting payment: %w", err)
	}

	var paidAt *time.Time
	if !resource.DateApproved.IsZero() {
		t := resource.DateApproved
		paidAt = &t
	}

	var moneyReleaseDate *time.Time
	if !resource.MoneyReleaseDate.IsZero() {
		t := resource.MoneyReleaseDate
		moneyReleaseDate = &t
	}

	amountCents := int64(resource.TransactionAmount * 100)
	fees, feeCents, netCents := extractFeeBreakdown(
		resource.FeeDetails,
		resource.TransactionDetails.NetReceivedAmount,
		amountCents,
	)

	status := &PaymentStatus{
		PaymentID:         strconv.Itoa(resource.ID),
		Status:            mapMPStatus(resource.Status),
		Amount:            amountCents, // currency units → cents
		PaidAt:            paidAt,
		FailureReason:     resource.StatusDetail,
		Metadata:          resource.Metadata,
		ExternalReference: resource.ExternalReference,
		PaymentMethod:     mapMPPaymentType(resource.PaymentTypeID),
		Installments:      resource.Installments,
		MoneyReleaseDate:  moneyReleaseDate,
		FeeAmountCents:    feeCents,
		NetAmountCents:    netCents,
		Fees:              fees,
	}

	// Audit log so we can reconcile gross/fee/net per payment without having
	// to re-call /v1/payments. `fees_breakdown` lists every line so a future
	// fee type (e.g. application_fee for marketplaces) shows up automatically.
	feesField := make([]map[string]any, 0, len(fees))
	for _, f := range fees {
		feesField = append(feesField, map[string]any{
			"type":         f.Type,
			"fee_payer":    f.FeePayer,
			"amount_cents": f.AmountCents,
		})
	}
	m.Logger.Info("mercado pago payment status fetched",
		zap.String("payment_id", status.PaymentID),
		zap.String("status", string(status.Status)),
		zap.String("status_detail", resource.StatusDetail),
		zap.String("payment_method", status.PaymentMethod),
		zap.Int("installments", status.Installments),
		zap.Int64("amount_cents", status.Amount),
		zap.Int64("fee_amount_cents", status.FeeAmountCents),
		zap.Int64("net_amount_cents", status.NetAmountCents),
		zap.Bool("has_money_release_date", moneyReleaseDate != nil),
		zap.Bool("has_paid_at", paidAt != nil),
		zap.Any("fees_breakdown", feesField),
	)

	return status, nil
}

// RefundPayment initiates a full or partial refund for a payment via the
// sdk-go Refund client.
func (m *MercadoPago) RefundPayment(ctx context.Context, paymentID string, amount *int64) (*RefundResult, error) {
	id, err := strconv.Atoi(paymentID)
	if err != nil {
		return nil, fmt.Errorf("invalid payment id %q: %w", paymentID, err)
	}

	cfg, err := m.sdkConfig()
	if err != nil {
		return nil, err
	}

	client := refund.NewClient(cfg)

	var resource *refund.Response
	if amount != nil {
		resource, err = client.CreatePartialRefund(ctx, id, float64(*amount)/100)
	} else {
		resource, err = client.Create(ctx, id)
	}
	if err != nil {
		return nil, fmt.Errorf("refunding payment: %w", err)
	}

	createdAt := resource.DateCreated

	return &RefundResult{
		RefundID:  strconv.Itoa(resource.ID),
		Status:    resource.Status,
		Amount:    int64(resource.Amount * 100), // currency units → cents
		CreatedAt: createdAt,
	}, nil
}

// sdkConfig builds a fresh sdk-go config bound to the current access token.
// We allocate per-call (instead of caching) because a) the token can rotate
// after a RefreshToken and a stale config would silently 401, and b) the
// allocation cost is trivial next to the network call that follows.
func (m *MercadoPago) sdkConfig(opts ...config.Option) (*config.Config, error) {
	cfg, err := config.New(m.credentials.AccessToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("init mp sdk config: %w", err)
	}
	return cfg, nil
}

// deviceIDRequester injects MP's anti-fraud X-meli-session-id header on
// every outbound request. The SDK exposes DeviceID as a body field on
// payment.Request, but /v1/payments rejects device_id in the body
// (`bad_request`: "The name of the following parameters is wrong:
// [device_id]"). MP only accepts the device fingerprint as a header, so we
// wire it via a wrapped Requester instead.
type deviceIDRequester struct {
	inner    *http.Client
	deviceID string
}

func (r *deviceIDRequester) Do(req *http.Request) (*http.Response, error) {
	if r.deviceID != "" {
		req.Header.Set("X-meli-session-id", r.deviceID)
	}
	return r.inner.Do(req)
}

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
// sdk-go Payment client. The SDK auto-attaches a fresh X-Idempotency-Key
// (random UUID) per request — that matches our prior behavior where each
// shopper attempt produced a fresh key, so a once-failed cart never gets
// stuck on MP's cached `internal_error`.
func (m *MercadoPago) ProcessCardPayment(ctx context.Context, input CardPaymentInput) (*CardPaymentResult, error) {
	// Inject the device fingerprint via the X-meli-session-id header. We do
	// NOT set request.DeviceID — /v1/payments rejects device_id as a body
	// param ("bad_request: [device_id]"), even though the SDK exposes it.
	cfg, err := m.sdkConfig(config.WithHTTPClient(&deviceIDRequester{
		inner:    &http.Client{Timeout: 30 * time.Second},
		deviceID: input.DeviceID,
	}))
	if err != nil {
		return nil, err
	}

	payer := buildPaymentPayer(input.Customer)

	req := payment.Request{
		TransactionAmount:   float64(input.TotalAmount) / 100, // cents → currency units
		Token:               input.Token,
		Installments:        input.Installments,
		Payer:               payer,
		ExternalReference:   input.CartID,
		NotificationURL:     input.NotifyURL,
		StatementDescriptor: "LIVECART",
		PaymentMethodID:     input.PaymentMethodID,
		IssuerID:            input.IssuerID,
		Metadata:            input.Metadata,
	}

	resource, err := payment.NewClient(cfg).Create(ctx, req)
	if err != nil {
		// MP rejections come back as *mperror.ResponseError. We log the raw
		// body + the request payload (token redacted) because MP's
		// `internal_error` often has an empty `cause` and only the exact
		// field set reveals the trigger (wrong payment_method_id,
		// malformed identification, etc.).
		var mpErr *mperror.ResponseError
		if errors.As(err, &mpErr) {
			safe := payment.Request{
				TransactionAmount:   req.TransactionAmount,
				Token:               "[redacted]",
				Installments:        req.Installments,
				Payer:               req.Payer,
				ExternalReference:   req.ExternalReference,
				NotificationURL:     req.NotificationURL,
				StatementDescriptor: req.StatementDescriptor,
				PaymentMethodID:     req.PaymentMethodID,
				IssuerID:            req.IssuerID,
				Metadata:            req.Metadata,
			}
			m.Logger.Error("mercado pago rejected card payment",
				zap.Int("status_code", mpErr.StatusCode),
				zap.String("body", mpErr.Message),
				zap.Any("request_payload", safe),
			)
			return &CardPaymentResult{
				Status:       PaymentRejected,
				StatusDetail: extractStatusDetail(mpErr.Message),
				Message:      mpErr.Message,
			}, nil
		}
		return nil, fmt.Errorf("processing card payment: %w", err)
	}

	result := &CardPaymentResult{
		PaymentID:         strconv.Itoa(resource.ID),
		Status:            mapMPStatus(resource.Status),
		StatusDetail:      resource.StatusDetail,
		Amount:            int64(resource.TransactionAmount * 100),
		Installments:      resource.Installments,
		CardBrand:         resource.PaymentMethodID,
		AuthorizationCode: resource.AuthorizationCode,
		ExternalReference: resource.ExternalReference,
		Message:           getStatusMessage(resource.Status, resource.StatusDetail),
	}

	if resource.Card.LastFourDigits != "" {
		result.LastFourDigits = resource.Card.LastFourDigits
	}

	if !resource.DateApproved.IsZero() {
		t := resource.DateApproved
		result.PaidAt = &t
	}

	// Capture and log the gross/fee/net breakdown the moment the payment is
	// created — by the time createFinalERPOrder runs (via webhook), the
	// numbers are the same, but logging here gives us a per-attempt audit
	// trail (paymentId + cart + breakdown) even when the merchant never
	// fetches the status again.
	amountCents := int64(resource.TransactionAmount * 100)
	fees, feeCents, netCents := extractFeeBreakdown(
		resource.FeeDetails,
		resource.TransactionDetails.NetReceivedAmount,
		amountCents,
	)
	feesField := make([]map[string]any, 0, len(fees))
	for _, f := range fees {
		feesField = append(feesField, map[string]any{
			"type":         f.Type,
			"fee_payer":    f.FeePayer,
			"amount_cents": f.AmountCents,
		})
	}
	var moneyReleaseDateStr string
	if !resource.MoneyReleaseDate.IsZero() {
		moneyReleaseDateStr = resource.MoneyReleaseDate.Format(time.RFC3339)
	}
	m.Logger.Info("mercado pago card payment captured",
		zap.String("payment_id", result.PaymentID),
		zap.String("cart_id", input.CartID),
		zap.String("status", string(result.Status)),
		zap.String("status_detail", resource.StatusDetail),
		zap.String("payment_method_id", resource.PaymentMethodID),
		zap.Int("installments", result.Installments),
		zap.Int64("amount_cents", amountCents),
		zap.Int64("fee_amount_cents", feeCents),
		zap.Int64("net_amount_cents", netCents),
		zap.String("money_release_date", moneyReleaseDateStr),
		zap.Bool("has_paid_at", result.PaidAt != nil),
		zap.Any("fees_breakdown", feesField),
	)

	return result, nil
}

// buildPaymentPayer maps our internal CheckoutCustomer onto the sdk-go
// PayerRequest. Shared between ProcessCardPayment and GeneratePixPayment.
func buildPaymentPayer(c CheckoutCustomer) *payment.PayerRequest {
	p := &payment.PayerRequest{Email: c.Email}
	if c.Name != "" {
		names := splitName(c.Name)
		p.FirstName = names[0]
		if len(names) > 1 {
			p.LastName = names[1]
		}
	}
	if c.Document != "" {
		p.Identification = &payment.IdentificationRequest{
			Type:   "CPF",
			Number: c.Document,
		}
	}
	if c.Phone != "" {
		p.Phone = &payment.PhoneRequest{Number: c.Phone}
	}
	if c.Address != nil {
		p.Address = &payment.AddressRequest{
			ZipCode:      c.Address.ZipCode,
			StreetName:   c.Address.Street,
			StreetNumber: c.Address.Number,
			Neighborhood: c.Address.Neighborhood,
			City:         c.Address.City,
			FederalUnit:  c.Address.State,
		}
	}
	return p
}

// extractStatusDetail tries to pull MP's `status_detail` out of the raw
// error body; falls back to "" so we still log a clean rejection.
func extractStatusDetail(body string) string {
	var parsed struct {
		StatusDetail string `json:"status_detail"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		return parsed.StatusDetail
	}
	return ""
}

// GeneratePixPayment generates a PIX QR code via the sdk-go Payment client.
// Behavior change vs. the previous raw HTTP call: the SDK auto-attaches a
// random X-Idempotency-Key per request, so a second PIX generation for the
// same cart issues a fresh QR instead of returning the cached one. This is
// safer (a re-attempt after expiry won't reuse the dead QR) and our
// cart-side idempotency already prevents accidental duplicates.
func (m *MercadoPago) GeneratePixPayment(ctx context.Context, input PixPaymentInput) (*PixPaymentResult, error) {
	cfg, err := m.sdkConfig()
	if err != nil {
		return nil, err
	}

	expiresIn := 30 * time.Minute
	if input.ExpiresIn != nil {
		expiresIn = *input.ExpiresIn
	}
	expiresAt := time.Now().Add(expiresIn)

	req := payment.Request{
		TransactionAmount: float64(input.TotalAmount) / 100,
		PaymentMethodID:   "pix",
		Payer:             buildPaymentPayer(input.Customer),
		ExternalReference: input.CartID,
		NotificationURL:   input.NotifyURL,
		DateOfExpiration:  &expiresAt,
		Metadata:          input.Metadata,
	}

	resource, err := payment.NewClient(cfg).Create(ctx, req)
	if err != nil {
		var mpErr *mperror.ResponseError
		if errors.As(err, &mpErr) {
			return nil, fmt.Errorf("pix generation failed: %s", mpErr.Message)
		}
		return nil, fmt.Errorf("generating pix payment: %w", err)
	}

	parsedExpiration := expiresAt
	if !resource.DateOfExpiration.IsZero() {
		parsedExpiration = resource.DateOfExpiration
	}

	return &PixPaymentResult{
		PaymentID:         strconv.Itoa(resource.ID),
		Status:            PaymentPending,
		QRCode:            resource.PointOfInteraction.TransactionData.QRCodeBase64,
		QRCodeText:        resource.PointOfInteraction.TransactionData.QRCode,
		Amount:            int64(resource.TransactionAmount * 100),
		ExpiresAt:         parsedExpiration,
		ExternalReference: resource.ExternalReference,
		TicketURL:         resource.PointOfInteraction.TransactionData.TicketURL,
	}, nil
}

// GetPaymentMethods returns the payment methods actually enabled on the
// collector's Mercado Pago account, via the sdk-go PaymentMethod client.
// Keeps only entries whose status is active. PIX is filtered out when the
// account hasn't registered a PIX key (or deactivated QR rendering), so the
// frontend never offers an option the merchant cannot fulfill.
//
// On any failure talking to Mercado Pago we fall back to ["card"] to keep
// the checkout working — we only offer PIX when we have positive evidence
// that it is enabled.
func (m *MercadoPago) GetPaymentMethods(ctx context.Context) ([]string, error) {
	cfg, err := m.sdkConfig()
	if err != nil {
		m.Logger.Warn("mercado pago payment_methods config init failed, falling back to card-only",
			zap.Error(err),
		)
		return []string{"card"}, nil
	}

	entries, err := paymentmethod.NewClient(cfg).List(ctx)
	if err != nil {
		m.Logger.Warn("mercado pago payment_methods lookup failed, falling back to card-only",
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

// extractFeeBreakdown turns the SDK fee_details slice + transaction_details
// block into the cents-based numbers we propagate downstream. We sum only
// fees where fee_payer == "collector" because those are what the merchant
// actually pays — payer-side fees show up on the buyer's invoice and don't
// affect contas a receber.
//
// Returns the per-fee list, total collector fees, and the net amount the
// merchant will receive (gateway-reported `net_received_amount`, or computed
// gross-fee when the gateway hasn't filled it yet — happens for pending
// payments).
func extractFeeBreakdown(
	feeDetails []payment.FeeDetailResponse,
	netReceivedAmount float64,
	transactionAmountCents int64,
) (fees []providers.PaymentFee, feeCents int64, netCents int64) {
	if len(feeDetails) > 0 {
		fees = make([]providers.PaymentFee, 0, len(feeDetails))
		for _, fd := range feeDetails {
			amountCents := int64(fd.Amount * 100)
			fees = append(fees, providers.PaymentFee{
				Type:        fd.Type,
				FeePayer:    fd.FeePayer,
				AmountCents: amountCents,
			})
			if fd.FeePayer == "collector" {
				feeCents += amountCents
			}
		}
	}
	netCents = int64(netReceivedAmount * 100)
	if netCents == 0 && transactionAmountCents > 0 && feeCents > 0 {
		// Pending/in_process payments often return fee_details but leave
		// net_received_amount at 0 — derive it so the audit log still adds
		// up. Once the payment settles the gateway will overwrite both.
		netCents = transactionAmountCents - feeCents
	}
	return fees, feeCents, netCents
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

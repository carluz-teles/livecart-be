package billing

// Minimal Stripe REST client (form-encoded + Basic auth), following the house
// pattern of hand-rolled provider clients (see providers/communication).
// Only the endpoints the billing flows need.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

const stripeAPIBaseURL = "https://api.stripe.com/v1"

// stripeGateway is the billing package's narrow view of the Stripe API.
// Defined here so tests can inject a stub without pulling in the concrete client.
type stripeGateway interface {
	CreateCustomer(ctx context.Context, storeID, storeName, email string) (*StripeCustomer, error)
	CreateTrialSubscription(ctx context.Context, customerID string, cfg PlanConfig, trialEnd time.Time) (*StripeSubscription, error)
	GetSubscription(ctx context.Context, subscriptionID string) (*StripeSubscription, error)
	CreateSetupCheckoutSession(ctx context.Context, customerID, successURL, cancelURL string, metadata map[string]string) (*CheckoutSession, error)
	CreateSubscriptionCheckoutSession(ctx context.Context, customerID, flatPriceID, successURL, cancelURL string, metadata map[string]string) (*CheckoutSession, error)
	GetSetupIntentPaymentMethod(ctx context.Context, setupIntentID string) (string, error)
	ActivateSubscription(ctx context.Context, sub *StripeSubscription, cfg PlanConfig, paymentMethodID string) (*StripeSubscription, error)
	MigrateSubscriptionItems(ctx context.Context, sub *StripeSubscription, priceID string) (*StripeSubscription, error)
	CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error)
	AddInvoiceItem(ctx context.Context, customerID, invoiceID string, amountCents int64, currency, description string) error
	CreateCustomerBalanceCredit(ctx context.Context, customerID string, amountCents int64, description string) error
}

// StripeClient talks to the Stripe API with the platform secret key.
type StripeClient struct {
	secretKey  string
	httpClient *http.Client
	logger     *zap.Logger
}

// NewStripeClient builds the client; returns nil when the key is absent so
// callers can degrade gracefully (local-only trial rows).
func NewStripeClient(secretKey string, logger *zap.Logger) *StripeClient {
	if secretKey == "" {
		return nil
	}
	return &StripeClient{
		secretKey:  secretKey,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		logger:     logger,
	}
}

// =============================================================================
// CUSTOMERS + SUBSCRIPTIONS
// =============================================================================

// StripeCustomer is the subset of the customer resource we consume.
type StripeCustomer struct {
	ID string `json:"id"`
}

// CreateCustomer registers the store as a Stripe customer.
func (c *StripeClient) CreateCustomer(ctx context.Context, storeID, storeName, email string) (*StripeCustomer, error) {
	form := url.Values{}
	form.Set("name", storeName)
	if email != "" {
		form.Set("email", email)
	}
	form.Set("metadata[store_id]", storeID)

	var out StripeCustomer
	if err := c.do(ctx, http.MethodPost, "/customers", form, &out); err != nil {
		return nil, fmt.Errorf("creating stripe customer: %w", err)
	}
	return &out, nil
}

// StripeSubscription is the subset of the subscription resource we consume.
type StripeSubscription struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	CancelAt           int64  `json:"cancel_at"` // timestamp — the portal schedules cancellation via this, not cancel_at_period_end
	TrialEnd           int64  `json:"trial_end"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	Customer           string `json:"customer"`
	DefaultPayment     string `json:"default_payment_method"`
	LatestInvoice      string `json:"latest_invoice"`
	Items              struct {
		Data []struct {
			ID                 string `json:"id"`
			CurrentPeriodStart int64  `json:"current_period_start"`
			CurrentPeriodEnd   int64  `json:"current_period_end"`
			Price              struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// PeriodStart returns the current cycle start. Recent Stripe API versions
// moved current_period_* to the subscription items (the top-level fields come
// back null) — fall back to the first item so webhooks keep the local period
// in sync regardless of the account's API version.
func (s *StripeSubscription) PeriodStart() int64 {
	if s.CurrentPeriodStart != 0 {
		return s.CurrentPeriodStart
	}
	if len(s.Items.Data) > 0 {
		return s.Items.Data[0].CurrentPeriodStart
	}
	return 0
}

// PeriodEnd returns the current cycle end (see PeriodStart for the item-level
// fallback rationale).
func (s *StripeSubscription) PeriodEnd() int64 {
	if s.CurrentPeriodEnd != 0 {
		return s.CurrentPeriodEnd
	}
	if len(s.Items.Data) > 0 {
		return s.Items.Data[0].CurrentPeriodEnd
	}
	return 0
}

// CreateTrialSubscription starts the cardless 7-day trial on the given plan.
// missing_payment_method=pause implements the paywall drop at trial end.
//
// Stripe constraint (verified live): pause-at-trial-end subscriptions can't
// carry metered items — so the trial holds ONLY the flat price. The metered
// GMV item is added at conversion, when the card arrives (trial GMV is free
// anyway).
func (c *StripeClient) CreateTrialSubscription(ctx context.Context, customerID string, cfg PlanConfig, trialEnd time.Time) (*StripeSubscription, error) {
	price, ok := cfg.Prices[IntervalMonthly]
	if !ok || price.PriceID == "" {
		return nil, fmt.Errorf("plan %s has no stripe price ids configured", cfg.Plan)
	}
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("items[0][price]", price.PriceID)
	form.Set("trial_end", strconv.FormatInt(trialEnd.Unix(), 10))
	form.Set("trial_settings[end_behavior][missing_payment_method]", "pause")
	form.Set("payment_settings[save_default_payment_method]", "on_subscription")

	var out StripeSubscription
	if err := c.do(ctx, http.MethodPost, "/subscriptions", form, &out); err != nil {
		return nil, fmt.Errorf("creating trial subscription: %w", err)
	}
	return &out, nil
}

// GetSubscription fetches the current remote state (used by reconciliation).
func (c *StripeClient) GetSubscription(ctx context.Context, subscriptionID string) (*StripeSubscription, error) {
	var out StripeSubscription
	if err := c.do(ctx, http.MethodGet, "/subscriptions/"+subscriptionID, nil, &out); err != nil {
		return nil, fmt.Errorf("fetching subscription: %w", err)
	}
	return &out, nil
}

// =============================================================================
// WEBHOOK SIGNATURE + EVENTS
// =============================================================================

// StripeEvent is the envelope of a webhook event; Data.Object stays raw so
// each handler decodes the type it expects.
type StripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// VerifyWebhookSignature implements Stripe's scheme: the Stripe-Signature
// header carries t=<ts>,v1=<hmac>; the signed payload is "<ts>.<body>" with
// HMAC-SHA256 keyed by the endpoint secret. 5-minute tolerance.
func VerifyWebhookSignature(payload []byte, header, secret string, now time.Time) bool {
	if header == "" || secret == "" {
		return false
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	if ts == "" || len(sigs) == 0 {
		return false
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(tsInt, 0)) > 5*time.Minute {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	for _, sig := range sigs {
		if hmac.Equal([]byte(expected), []byte(sig)) {
			return true
		}
	}
	return false
}

// =============================================================================
// HTTP
// =============================================================================

type stripeError struct {
	Err struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *StripeClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	return c.doIdem(ctx, method, path, form, "", out)
}

// doIdem is do with an optional Stripe Idempotency-Key — set it on writes that
// must not duplicate on retry/webhook-redelivery (e.g. commission invoice items).
func (c *StripeClient) doIdem(ctx context.Context, method, path string, form url.Values, idemKey string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, stripeAPIBaseURL+path, body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(c.secretKey, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	logger.From(ctx, c.logger).Debug("stripe request",
		zap.String("method", method),
		zap.String("path", path),
		zap.Duration("duration", time.Since(start)),
	)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var se stripeError
		if json.Unmarshal(raw, &se) == nil && se.Err.Message != "" {
			return fmt.Errorf("stripe %s (%s): %s", se.Err.Type, se.Err.Code, se.Err.Message)
		}
		return fmt.Errorf("stripe http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}
	}
	return nil
}

// =============================================================================
// CONVERSION (trial → active)
// =============================================================================

// CheckoutSession is the subset of the session resource we consume.
type CheckoutSession struct {
	ID           string            `json:"id"`
	URL          string            `json:"url"`
	Mode         string            `json:"mode"`
	Customer     string            `json:"customer"`
	Metadata     map[string]string `json:"metadata"`
	SetupIntent  string            `json:"setup_intent"`
	Subscription string            `json:"subscription"` // populated in subscription mode
}

// StripeInvoice is the subset of the invoice resource the cycle reactor consumes.
type StripeInvoice struct {
	ID            string `json:"id"`
	BillingReason string `json:"billing_reason"`
	Subscription  string `json:"subscription"` // legacy top-level; empty on 2025+ API versions
	Customer      string `json:"customer"`
	PeriodStart   int64  `json:"period_start"`
	PeriodEnd     int64  `json:"period_end"`
	Parent        struct {
		SubscriptionDetails struct {
			Subscription string `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// SubscriptionID resolves the subscription reference across Stripe API versions:
// 2025+ drops the top-level invoice.subscription and exposes it under
// parent.subscription_details.subscription instead.
func (i *StripeInvoice) SubscriptionID() string {
	if i.Subscription != "" {
		return i.Subscription
	}
	return i.Parent.SubscriptionDetails.Subscription
}

// CreateSetupCheckoutSession opens a hosted Checkout that collects a payment
// method for an existing (trialing/paused) subscription. Conversion itself
// happens on the checkout.session.completed webhook.
func (c *StripeClient) CreateSetupCheckoutSession(ctx context.Context, customerID, successURL, cancelURL string, metadata map[string]string) (*CheckoutSession, error) {
	form := url.Values{}
	form.Set("mode", "setup")
	form.Set("customer", customerID)
	form.Set("currency", "brl")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	for k, v := range metadata {
		form.Set("metadata["+k+"]", v)
	}

	var out CheckoutSession
	if err := c.do(ctx, http.MethodPost, "/checkout/sessions", form, &out); err != nil {
		return nil, fmt.Errorf("creating checkout session: %w", err)
	}
	return &out, nil
}

// CreateSubscriptionCheckoutSession opens a hosted Checkout in subscription
// mode that creates the paid subscription directly (the trial-local model:
// there is no pre-existing Stripe subscription). The single Pro price for the
// chosen interval is the only line item. allow_promotion_codes exposes
// Stripe's native promo field (e.g. CANTODAART). No GMV fee to disclose
// anymore — the commission was eliminated for everyone.
//
// Follow-up (needs a Terms of Service URL configured in Stripe branding):
// consent_collection[terms_of_service]=required to capture a recorded acceptance.
func (c *StripeClient) CreateSubscriptionCheckoutSession(ctx context.Context, customerID, flatPriceID, successURL, cancelURL string, metadata map[string]string) (*CheckoutSession, error) {
	if flatPriceID == "" {
		return nil, fmt.Errorf("subscription checkout requires a price id")
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("customer", customerID)
	form.Set("line_items[0][price]", flatPriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("allow_promotion_codes", "true")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	// Carry the identifiers on both the session and the created subscription so
	// the webhook/reactor can resolve store+plan without a side lookup.
	for k, v := range metadata {
		form.Set("metadata["+k+"]", v)
		form.Set("subscription_data[metadata]["+k+"]", v)
	}

	var out CheckoutSession
	if err := c.do(ctx, http.MethodPost, "/checkout/sessions", form, &out); err != nil {
		return nil, fmt.Errorf("creating subscription checkout session: %w", err)
	}
	return &out, nil
}

// GetSetupIntentPaymentMethod resolves the payment method collected by a
// setup-mode Checkout.
func (c *StripeClient) GetSetupIntentPaymentMethod(ctx context.Context, setupIntentID string) (string, error) {
	var out struct {
		PaymentMethod string `json:"payment_method"`
	}
	if err := c.do(ctx, http.MethodGet, "/setup_intents/"+setupIntentID, nil, &out); err != nil {
		return "", fmt.Errorf("fetching setup intent: %w", err)
	}
	return out.PaymentMethod, nil
}

// ActivateSubscription converts the trial: attaches the payment method, swaps
// the item to the chosen plan/interval price and bills the first invoice
// immediately. There is no more metered item — the GMV commission was
// eliminated for everyone.
//
// This is the legacy setup-mode conversion path (stores that already carried
// a Stripe trial subscription before the trial-local model). It has no
// interval information available at this point, so it assumes the monthly
// price — merchants can switch interval afterwards via the Customer Portal.
//
// Stripe blocks several of these updates while the pause-at-trial-end
// behavior is set or the subscription is already paused (constraints
// verified against the test API):
//   - the pause validator runs against the PRE-update state, so
//     trial_settings must be cleared in its own request first;
//   - payment_behavior is rejected while paused — the paused path goes
//     through resume instead (activatePausedSubscription).
func (c *StripeClient) ActivateSubscription(ctx context.Context, sub *StripeSubscription, cfg PlanConfig, paymentMethodID string) (*StripeSubscription, error) {
	price, ok := cfg.Prices[IntervalMonthly]
	if !ok || price.PriceID == "" {
		return nil, fmt.Errorf("plan %s has no stripe price ids configured", cfg.Plan)
	}
	if sub.Status == "paused" {
		return c.activatePausedSubscription(ctx, sub, price.PriceID, paymentMethodID)
	}

	clear := url.Values{}
	clear.Set("trial_settings[end_behavior][missing_payment_method]", "create_invoice")
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, clear, nil); err != nil {
		return nil, fmt.Errorf("clearing trial pause behavior: %w", err)
	}

	form := url.Values{}
	form.Set("default_payment_method", paymentMethodID)
	if sub.Status == "trialing" {
		form.Set("trial_end", "now")
	}
	form.Set("proration_behavior", "none")
	form.Set("payment_behavior", "allow_incomplete")
	if len(sub.Items.Data) > 0 {
		form.Set("items[0][id]", sub.Items.Data[0].ID)
	}
	form.Set("items[0][price]", price.PriceID)

	var out StripeSubscription
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, form, &out); err != nil {
		return nil, fmt.Errorf("activating subscription: %w", err)
	}
	return &out, nil
}

// activatePausedSubscription converts a subscription paused at trial end (the
// merchant let the trial expire and is paying through the paywall). Sequence
// required by Stripe: card + price swap while paused, then resume on a fresh
// cycle and pay the resumption invoice.
func (c *StripeClient) activatePausedSubscription(ctx context.Context, sub *StripeSubscription, priceID, paymentMethodID string) (*StripeSubscription, error) {
	form := url.Values{}
	form.Set("default_payment_method", paymentMethodID)
	if len(sub.Items.Data) > 0 {
		form.Set("items[0][id]", sub.Items.Data[0].ID)
	}
	form.Set("items[0][price]", priceID)
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, form, nil); err != nil {
		return nil, fmt.Errorf("preparing paused subscription: %w", err)
	}

	var resumed StripeSubscription
	resume := url.Values{}
	resume.Set("billing_cycle_anchor", "now")
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID+"/resume", resume, &resumed); err != nil {
		return nil, fmt.Errorf("resuming subscription: %w", err)
	}

	// The resumption invoice finalizes as open and Stripe collects it
	// asynchronously; pay it now so access doesn't wait (nor depend on) the
	// async attempt.
	if resumed.Status != "active" && resumed.LatestInvoice != "" {
		if err := c.do(ctx, http.MethodPost, "/invoices/"+resumed.LatestInvoice+"/pay", nil, nil); err != nil {
			return nil, fmt.Errorf("paying resumption invoice: %w", err)
		}
	}

	form = url.Values{}
	form.Set("trial_settings[end_behavior][missing_payment_method]", "create_invoice")
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, form, nil); err != nil {
		return nil, fmt.Errorf("clearing trial pause behavior after resume: %w", err)
	}
	return &resumed, nil
}

// MigrateSubscriptionItems replaces every item on a legacy subscription
// (flat + metered, from the old 3-plan model) with a single item on the new
// Pro price, WITHOUT proration and WITHOUT moving the billing_cycle_anchor —
// the store keeps its existing renewal date and simply starts being charged
// the new flat price (no commission) from that date on. Used by the one-off
// pricing migration (cmd/migrate-billing-plan), never by the regular
// checkout/webhook flows.
func (c *StripeClient) MigrateSubscriptionItems(ctx context.Context, sub *StripeSubscription, priceID string) (*StripeSubscription, error) {
	form := url.Values{}
	form.Set("proration_behavior", "none")
	if len(sub.Items.Data) == 0 {
		return nil, fmt.Errorf("subscription %s has no items to migrate", sub.ID)
	}
	// Keep item 0 alive on the new price; delete every extra item (the old
	// metered GMV item, when present).
	form.Set("items[0][id]", sub.Items.Data[0].ID)
	form.Set("items[0][price]", priceID)
	for i, item := range sub.Items.Data[1:] {
		idx := i + 1
		form.Set(fmt.Sprintf("items[%d][id]", idx), item.ID)
		form.Set(fmt.Sprintf("items[%d][deleted]", idx), "true")
	}

	var out StripeSubscription
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, form, &out); err != nil {
		return nil, fmt.Errorf("migrating subscription items: %w", err)
	}
	return &out, nil
}

// =============================================================================
// PORTAL
// =============================================================================

// CreatePortalSession opens the Stripe Customer Portal (card, invoices,
// cancellation).
func (c *StripeClient) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("return_url", returnURL)

	var out struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodPost, "/billing_portal/sessions", form, &out); err != nil {
		return "", fmt.Errorf("creating portal session: %w", err)
	}
	return out.URL, nil
}

// =============================================================================
// USAGE (Billing Meters — GMV fee)
// =============================================================================

// SendMeterEvent reports paid GMV for metered billing. identifier dedupes on
// Stripe's side (retries of the same cart are no-ops).
// AddInvoiceItem attaches a one-off charge to a customer's invoice — used to
// bill the accrued GMV commission on each renewal invoice, so mensalidade and
// commission land on ONE charge. amountCents may be negative (a credit). The
// idempotency key (derived from the invoice) makes a webhook redelivery a
// no-op even if the caller's "mark invoiced" step failed on the first attempt.
func (c *StripeClient) AddInvoiceItem(ctx context.Context, customerID, invoiceID string, amountCents int64, currency, description string) error {
	if customerID == "" {
		return fmt.Errorf("invoice item requires a customer id")
	}
	form := url.Values{}
	form.Set("customer", customerID)
	if invoiceID != "" {
		form.Set("invoice", invoiceID)
	}
	form.Set("amount", strconv.FormatInt(amountCents, 10))
	form.Set("currency", currency)
	if description != "" {
		form.Set("description", description)
	}
	idemKey := ""
	if invoiceID != "" {
		idemKey = "gmv-commission-" + invoiceID
	}
	if err := c.doIdem(ctx, http.MethodPost, "/invoiceitems", form, idemKey, nil); err != nil {
		return fmt.Errorf("creating invoice item: %w", err)
	}
	return nil
}

// CreateCustomerBalanceCredit adds a negative balance transaction — Stripe
// automatically deducts it from the customer's next finalized invoice. Used
// to reimburse the GMV fee when a sale is refunded (PRD 007 §refunds), which
// also covers cross-cycle refunds (sale billed on cycle 1, refunded on
// cycle 2 → credit lands on cycle 2's invoice).
func (c *StripeClient) CreateCustomerBalanceCredit(ctx context.Context, customerID string, amountCents int64, description string) error {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(-amountCents, 10)) // negativo = crédito
	form.Set("currency", "brl")
	form.Set("description", description)

	if err := c.do(ctx, http.MethodPost, "/customers/"+customerID+"/balance_transactions", form, nil); err != nil {
		return fmt.Errorf("creating balance credit: %w", err)
	}
	return nil
}

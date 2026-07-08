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
)

const stripeAPIBaseURL = "https://api.stripe.com/v1"

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
	TrialEnd           int64  `json:"trial_end"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	Customer           string `json:"customer"`
	DefaultPayment     string `json:"default_payment_method"`
	Items              struct {
		Data []struct {
			ID    string `json:"id"`
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// CreateTrialSubscription starts the cardless 7-day trial on the given plan.
// missing_payment_method=pause implements the paywall drop at trial end.
//
// Stripe constraint (verified live): pause-at-trial-end subscriptions can't
// carry metered items — so the trial holds ONLY the flat price. The metered
// GMV item is added at conversion, when the card arrives (trial GMV is free
// anyway).
func (c *StripeClient) CreateTrialSubscription(ctx context.Context, customerID string, cfg PlanConfig, trialEnd time.Time) (*StripeSubscription, error) {
	if cfg.FlatPriceID == "" {
		return nil, fmt.Errorf("plan %s has no stripe price ids configured", cfg.Plan)
	}
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("items[0][price]", cfg.FlatPriceID)
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

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	c.logger.Debug("stripe request",
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
	ID          string            `json:"id"`
	URL         string            `json:"url"`
	Mode        string            `json:"mode"`
	Customer    string            `json:"customer"`
	Metadata    map[string]string `json:"metadata"`
	SetupIntent string            `json:"setup_intent"`
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
// the flat item to the chosen plan, adds the metered GMV item (allowed now —
// the pause constraint only applies to trials without a card) and ends the
// trial so the first flat invoice bills immediately.
func (c *StripeClient) ActivateSubscription(ctx context.Context, sub *StripeSubscription, cfg PlanConfig, paymentMethodID string) (*StripeSubscription, error) {
	if cfg.FlatPriceID == "" || cfg.MeterPriceID == "" {
		return nil, fmt.Errorf("plan %s has no stripe price ids configured", cfg.Plan)
	}

	form := url.Values{}
	form.Set("default_payment_method", paymentMethodID)
	form.Set("trial_end", "now")
	form.Set("proration_behavior", "none")
	form.Set("payment_behavior", "allow_incomplete")

	// Swap the existing flat item and append the metered one.
	if len(sub.Items.Data) > 0 {
		form.Set("items[0][id]", sub.Items.Data[0].ID)
		form.Set("items[0][price]", cfg.FlatPriceID)
		form.Set("items[1][price]", cfg.MeterPriceID)
	} else {
		form.Set("items[0][price]", cfg.FlatPriceID)
		form.Set("items[1][price]", cfg.MeterPriceID)
	}

	var out StripeSubscription
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, form, &out); err != nil {
		return nil, fmt.Errorf("activating subscription: %w", err)
	}
	return &out, nil
}

// =============================================================================
// PORTAL + PLAN CHANGES
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

// UpgradeSubscription applies the new plan immediately, invoicing the
// prorated flat difference now (PRD 007: upgrade imediato com proration).
func (c *StripeClient) UpgradeSubscription(ctx context.Context, sub *StripeSubscription, cfg PlanConfig) (*StripeSubscription, error) {
	form := url.Values{}
	form.Set("proration_behavior", "always_invoice")
	form.Set("payment_behavior", "allow_incomplete")

	flatID, meterID := findItemIDs(sub, cfg)
	i := 0
	if flatID != "" {
		form.Set(fmt.Sprintf("items[%d][id]", i), flatID)
		form.Set(fmt.Sprintf("items[%d][price]", i), cfg.FlatPriceID)
		i++
	} else {
		form.Set(fmt.Sprintf("items[%d][price]", i), cfg.FlatPriceID)
		i++
	}
	if meterID != "" {
		form.Set(fmt.Sprintf("items[%d][id]", i), meterID)
		form.Set(fmt.Sprintf("items[%d][price]", i), cfg.MeterPriceID)
	} else {
		form.Set(fmt.Sprintf("items[%d][price]", i), cfg.MeterPriceID)
	}

	var out StripeSubscription
	if err := c.do(ctx, http.MethodPost, "/subscriptions/"+sub.ID, form, &out); err != nil {
		return nil, fmt.Errorf("upgrading subscription: %w", err)
	}
	return &out, nil
}

// ScheduleDowngrade swaps the plan at the end of the current period via a
// subscription schedule (PRD 007: downgrade agendado, sem estorno).
func (c *StripeClient) ScheduleDowngrade(ctx context.Context, sub *StripeSubscription, cfg PlanConfig) error {
	// 1. Wrap the subscription in a schedule.
	form := url.Values{}
	form.Set("from_subscription", sub.ID)
	var schedule struct {
		ID     string `json:"id"`
		Phases []struct {
			StartDate int64 `json:"start_date"`
			EndDate   int64 `json:"end_date"`
		} `json:"phases"`
	}
	if err := c.do(ctx, http.MethodPost, "/subscription_schedules", form, &schedule); err != nil {
		// Already managed by a schedule — surface a friendly error; the
		// merchant can retry after the pending change resolves.
		return fmt.Errorf("creating subscription schedule: %w", err)
	}

	// 2. Keep the current phase as-is and append the downgraded phase.
	update := url.Values{}
	update.Set("end_behavior", "release")
	// current phase: preserve existing prices until period end
	update.Set("phases[0][start_date]", strconv.FormatInt(schedule.Phases[0].StartDate, 10))
	update.Set("phases[0][end_date]", strconv.FormatInt(sub.CurrentPeriodEnd, 10))
	for i, item := range sub.Items.Data {
		update.Set(fmt.Sprintf("phases[0][items][%d][price]", i), item.Price.ID)
	}
	// next phase: new plan prices
	update.Set("phases[1][items][0][price]", cfg.FlatPriceID)
	update.Set("phases[1][items][1][price]", cfg.MeterPriceID)

	if err := c.do(ctx, http.MethodPost, "/subscription_schedules/"+schedule.ID, update, nil); err != nil {
		return fmt.Errorf("scheduling downgrade phases: %w", err)
	}
	return nil
}

// findItemIDs maps the subscription's current items to flat/meter slots by
// matching against any known plan price.
func findItemIDs(sub *StripeSubscription, _ PlanConfig) (flatItemID, meterItemID string) {
	for _, item := range sub.Items.Data {
		for _, cfg := range Plans() {
			if item.Price.ID == cfg.FlatPriceID {
				flatItemID = item.ID
			}
			if item.Price.ID == cfg.MeterPriceID {
				meterItemID = item.ID
			}
		}
	}
	return flatItemID, meterItemID
}

// =============================================================================
// USAGE (Billing Meters — GMV fee)
// =============================================================================

// SendMeterEvent reports paid GMV for metered billing. identifier dedupes on
// Stripe's side (retries of the same cart are no-ops).
func (c *StripeClient) SendMeterEvent(ctx context.Context, eventName, customerID, identifier string, valueCents int64) error {
	form := url.Values{}
	form.Set("event_name", eventName)
	form.Set("identifier", identifier)
	form.Set("payload[stripe_customer_id]", customerID)
	form.Set("payload[value]", strconv.FormatInt(valueCents, 10))

	if err := c.do(ctx, http.MethodPost, "/billing/meter_events", form, nil); err != nil {
		return fmt.Errorf("sending meter event: %w", err)
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

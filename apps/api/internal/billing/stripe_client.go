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

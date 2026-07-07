// Package communication contains providers for merchant→customer messaging
// channels (WhatsApp via Twilio — PRD 006).
package communication

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

const (
	twilioAPIBaseURL       = "https://api.twilio.com/2010-04-01"
	twilioMessagingBaseURL = "https://messaging.twilio.com/v2"
)

// Metadata keys stored on the integration row (see PRD 006 §6.1).
const (
	MetaPhoneNumber        = "phone_number"         // merchant number in E.164
	MetaSenderSID          = "sender_sid"           // Twilio WhatsApp sender (XE...)
	MetaWABAID             = "waba_id"              // Meta WhatsApp Business Account
	MetaContentSIDRecovery = "content_sid_recovery" // approved cart-recovery template
)

// Twilio implements the CommunicationProvider interface for WhatsApp via
// Twilio. Each store maps to a Twilio subaccount whose SID/token live in the
// integration credentials (APIKey/APISecret).
type Twilio struct {
	*providers.BaseProvider

	accountSID string // subaccount SID (per store)
	authToken  string // subaccount auth token
	fromNumber string // merchant WhatsApp number, E.164
	senderSID  string // Twilio sender resource (XE...), set after onboarding
}

// NewTwilio creates a new Twilio WhatsApp provider. Signature matches
// providers.TwilioConstructor.
func NewTwilio(cfg providers.TwilioConfig) (*Twilio, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("credentials are required")
	}
	if cfg.Credentials.APIKey == "" || cfg.Credentials.APISecret == "" {
		return nil, fmt.Errorf("api_key (account SID) and api_secret (auth token) are required")
	}

	fromNumber, _ := cfg.Metadata[MetaPhoneNumber].(string)
	senderSID, _ := cfg.Metadata[MetaSenderSID].(string)

	return &Twilio{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       15 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		accountSID: cfg.Credentials.APIKey,
		authToken:  cfg.Credentials.APISecret,
		fromNumber: fromNumber,
		senderSID:  senderSID,
	}, nil
}

// =============================================================================
// PROVIDER INTERFACE
// =============================================================================

func (t *Twilio) Type() providers.ProviderType { return providers.ProviderTypeCommunication }
func (t *Twilio) Name() providers.ProviderName { return providers.ProviderTwilioWhatsApp }

// RefreshToken is a no-op: Twilio uses static credentials.
func (t *Twilio) RefreshToken(_ context.Context) (*providers.Credentials, error) {
	return nil, nil
}

// ValidateCredentials fetches the account resource to prove SID+token work.
func (t *Twilio) ValidateCredentials(ctx context.Context) error {
	endpoint := fmt.Sprintf("%s/Accounts/%s.json", twilioAPIBaseURL, t.accountSID)
	body, err := t.doGet(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("validating twilio credentials: %w", err)
	}
	var acc struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &acc); err != nil {
		return fmt.Errorf("parsing account response: %w", err)
	}
	if acc.Status == "suspended" || acc.Status == "closed" {
		return fmt.Errorf("twilio account is %s", acc.Status)
	}
	return nil
}

// TestConnection reports account status and, when a sender is registered,
// the WhatsApp sender health.
func (t *Twilio) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	result := &providers.TestConnectionResult{TestedAt: start}

	if err := t.ValidateCredentials(ctx); err != nil {
		result.Success = false
		result.Message = err.Error()
		result.Latency = time.Since(start)
		return result, nil
	}

	info := map[string]any{"account_sid": t.accountSID}
	if t.senderSID != "" {
		if status, err := t.GetSenderStatus(ctx); err == nil {
			info["sender_status"] = status.Status
			info["phone_number"] = status.PhoneNumber
			info["quality_rating"] = status.QualityRating
		}
	}

	result.Success = true
	result.Message = "Conexão com a Twilio OK"
	result.Latency = time.Since(start)
	result.AccountInfo = info
	return result, nil
}

// =============================================================================
// COMMUNICATION PROVIDER INTERFACE
// =============================================================================

// SendTemplateMessage sends an approved content template to a WhatsApp
// destination. Twilio expects application/x-www-form-urlencoded.
func (t *Twilio) SendTemplateMessage(ctx context.Context, msg providers.TemplateMessage) (*providers.MessageResult, error) {
	if t.fromNumber == "" {
		return nil, fmt.Errorf("whatsapp sender number not configured (metadata %q)", MetaPhoneNumber)
	}
	if msg.ContentSid == "" {
		return nil, fmt.Errorf("content sid is required for business-initiated messages")
	}

	vars := "{}"
	if len(msg.Variables) > 0 {
		raw, err := json.Marshal(msg.Variables)
		if err != nil {
			return nil, fmt.Errorf("marshaling content variables: %w", err)
		}
		vars = string(raw)
	}

	form := url.Values{}
	form.Set("To", "whatsapp:"+msg.To)
	form.Set("From", "whatsapp:"+t.fromNumber)
	form.Set("ContentSid", msg.ContentSid)
	form.Set("ContentVariables", vars)
	if msg.CallbackURL != "" {
		form.Set("StatusCallback", msg.CallbackURL)
	}

	endpoint := fmt.Sprintf("%s/Accounts/%s/Messages.json", twilioAPIBaseURL, t.accountSID)
	body, err := t.doForm(ctx, http.MethodPost, endpoint, form)
	if err != nil {
		return nil, err
	}

	var out struct {
		Sid    string `json:"sid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing message response: %w", err)
	}

	return &providers.MessageResult{MessageID: out.Sid, Status: out.Status}, nil
}

// GetSenderStatus fetches the WhatsApp sender resource (merchant number).
func (t *Twilio) GetSenderStatus(ctx context.Context) (*providers.SenderStatus, error) {
	if t.senderSID == "" {
		return &providers.SenderStatus{Status: "NOT_REGISTERED", PhoneNumber: t.fromNumber, QualityRating: "UNKNOWN"}, nil
	}

	endpoint := fmt.Sprintf("%s/Channels/Senders/%s", twilioMessagingBaseURL, t.senderSID)
	body, err := t.doGet(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("fetching sender status: %w", err)
	}

	var out struct {
		Status     string `json:"status"`
		SenderID   string `json:"sender_id"` // "whatsapp:+55..."
		Properties struct {
			QualityRating string `json:"quality_rating"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing sender response: %w", err)
	}

	phone := strings.TrimPrefix(out.SenderID, "whatsapp:")
	quality := out.Properties.QualityRating
	if quality == "" {
		quality = "UNKNOWN"
	}
	return &providers.SenderStatus{Status: out.Status, PhoneNumber: phone, QualityRating: quality}, nil
}

// =============================================================================
// HTTP HELPERS (Twilio is form-encoded + basic auth; BaseProvider.DoRequest
// is JSON-only, so we roll our own using the same client/logging)
// =============================================================================

func (t *Twilio) doGet(ctx context.Context, endpoint string) ([]byte, error) {
	return t.do(ctx, http.MethodGet, endpoint, "", nil)
}

func (t *Twilio) doForm(ctx context.Context, method, endpoint string, form url.Values) ([]byte, error) {
	return t.do(ctx, method, endpoint, form.Encode(), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
}

func (t *Twilio) do(ctx context.Context, method, endpoint, body string, headers map[string]string) ([]byte, error) {
	if t.RateLimiter != nil {
		if err := t.RateLimiter.Wait(ctx); err != nil {
			return nil, err
		}
	}

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(t.accountSID, t.authToken)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := t.HTTPClient.Do(req)
	t.Logger.Debug("twilio http request",
		zap.String("integration_id", t.IntegrationID),
		zap.String("method", method),
		zap.String("url", endpoint),
		zap.Duration("duration", time.Since(start)),
	)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, parseTwilioError(resp.StatusCode, respBody)
	}
	return respBody, nil
}

// TwilioError is a structured Twilio API error (code list:
// https://www.twilio.com/docs/api/errors).
type TwilioError struct {
	HTTPStatus int
	Code       int    `json:"code"`
	Message    string `json:"message"`
}

func (e *TwilioError) Error() string {
	return fmt.Sprintf("twilio error %d (http %d): %s", e.Code, e.HTTPStatus, e.Message)
}

func parseTwilioError(status int, body []byte) error {
	te := &TwilioError{HTTPStatus: status}
	if err := json.Unmarshal(body, te); err != nil || te.Message == "" {
		te.Message = strings.TrimSpace(string(body))
		if te.Message == "" {
			te.Message = http.StatusText(status)
		}
	}
	return te
}

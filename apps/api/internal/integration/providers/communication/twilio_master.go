package communication

// MasterClient wraps master-account Twilio operations used during merchant
// onboarding (PRD 006 sprint 2): subaccount creation, WhatsApp sender
// registration/verification (merchant's own number, OTP by SMS/voice) and
// content template provisioning + WhatsApp approval.
//
// Per Twilio's ISV model, ONE subaccount maps to ONE WABA/merchant. Message
// sending afterwards happens through the per-store provider (twilio.go).

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
)

const twilioContentBaseURL = "https://content.twilio.com/v1"

// MasterClient talks to Twilio with the master account credentials.
type MasterClient struct {
	accountSID string
	authToken  string
	httpClient *http.Client
	logger     *zap.Logger
}

// NewMasterClient creates a master-level Twilio client.
func NewMasterClient(accountSID, authToken string, logger *zap.Logger) (*MasterClient, error) {
	if accountSID == "" || authToken == "" {
		return nil, fmt.Errorf("twilio master credentials not configured")
	}
	return &MasterClient{
		accountSID: accountSID,
		authToken:  authToken,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		logger:     logger,
	}, nil
}

// =============================================================================
// SUBACCOUNTS
// =============================================================================

// Subaccount is a Twilio subaccount created for a store.
type Subaccount struct {
	SID       string `json:"sid"`
	AuthToken string `json:"auth_token"`
	Status    string `json:"status"`
}

// CreateSubaccount provisions a subaccount for a store (idempotent by
// friendly name is NOT guaranteed by Twilio — caller must persist the SID).
func (m *MasterClient) CreateSubaccount(ctx context.Context, friendlyName string) (*Subaccount, error) {
	form := url.Values{}
	form.Set("FriendlyName", friendlyName)

	body, err := m.doForm(ctx, http.MethodPost, twilioAPIBaseURL+"/Accounts.json", form, m.accountSID, m.authToken)
	if err != nil {
		return nil, fmt.Errorf("creating subaccount: %w", err)
	}

	var out Subaccount
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing subaccount response: %w", err)
	}
	return &out, nil
}

// =============================================================================
// WHATSAPP SENDERS (merchant's own number)
// =============================================================================

// Sender is a WhatsApp sender resource.
type Sender struct {
	SID      string `json:"sid"`
	SenderID string `json:"sender_id"` // "whatsapp:+55..."
	Status   string `json:"status"`    // CREATING | PENDING_VERIFICATION | VERIFYING | ONLINE | ...
}

// RegisterSenderInput registers the merchant number as a WhatsApp sender on
// a subaccount. WABA ID comes from the embedded-signup step.
type RegisterSenderInput struct {
	SubaccountSID   string
	SubaccountToken string
	PhoneE164       string // +5511999999999
	WABAID          string
	DisplayName     string // business profile name shown on WhatsApp
	CallbackURL     string // inbound/status webhook
}

// RegisterSender starts sender registration. Twilio/Meta then sends an OTP
// (SMS or voice) to the number; complete with VerifySender.
func (m *MasterClient) RegisterSender(ctx context.Context, input RegisterSenderInput) (*Sender, error) {
	payload := map[string]any{
		"sender_id": "whatsapp:" + input.PhoneE164,
		"webhook": map[string]any{
			"callback_url":    input.CallbackURL,
			"callback_method": "POST",
		},
	}
	if input.WABAID != "" {
		payload["configuration"] = map[string]any{"waba_id": input.WABAID}
	}
	if input.DisplayName != "" {
		payload["profile"] = map[string]any{"name": input.DisplayName}
	}

	body, err := m.doJSON(ctx, http.MethodPost, twilioMessagingBaseURL+"/Channels/Senders", payload, input.SubaccountSID, input.SubaccountToken)
	if err != nil {
		return nil, fmt.Errorf("registering whatsapp sender: %w", err)
	}

	var out Sender
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing sender response: %w", err)
	}
	return &out, nil
}

// VerifySender submits the OTP the merchant received on their phone.
func (m *MasterClient) VerifySender(ctx context.Context, subSID, subToken, senderSID, code string) (*Sender, error) {
	payload := map[string]any{
		"configuration": map[string]any{"verification_code": code},
	}

	body, err := m.doJSON(ctx, http.MethodPost, twilioMessagingBaseURL+"/Channels/Senders/"+senderSID, payload, subSID, subToken)
	if err != nil {
		return nil, fmt.Errorf("verifying whatsapp sender: %w", err)
	}

	var out Sender
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing sender response: %w", err)
	}
	return &out, nil
}

// =============================================================================
// CONTENT TEMPLATES (Content API) + WHATSAPP APPROVAL
// =============================================================================

// ContentTemplate is a Twilio Content resource (HX...).
type ContentTemplate struct {
	SID string `json:"sid"`
}

// CreateRecoveryTemplate creates the default cart-recovery template on the
// store's subaccount. Variables: {{1}} customer name, {{2}} items summary,
// {{3}} checkout link.
func (m *MasterClient) CreateRecoveryTemplate(ctx context.Context, subSID, subToken, storeName string) (*ContentTemplate, error) {
	payload := map[string]any{
		"friendly_name": "livecart_cart_recovery",
		"language":      "pt_BR",
		"variables":     map[string]string{"1": "Cliente", "2": "2 itens · R$ 119,80", "3": "https://livecart.app/cart/exemplo"},
		"types": map[string]any{
			"twilio/text": map[string]any{
				"body": fmt.Sprintf("Oi {{1}}! Vi que seu carrinho na %s ficou pra trás 🛒\n\n{{2}}\n\nGaranti tudo de novo pra você. Finalize aqui: {{3}}", storeName),
			},
		},
	}

	body, err := m.doJSON(ctx, http.MethodPost, twilioContentBaseURL+"/Content", payload, subSID, subToken)
	if err != nil {
		return nil, fmt.Errorf("creating content template: %w", err)
	}

	var out ContentTemplate
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing content response: %w", err)
	}
	return &out, nil
}

// SubmitTemplateApproval submits a content template for WhatsApp approval.
// Cart recovery with promotional tone is classified as MARKETING by Meta.
func (m *MasterClient) SubmitTemplateApproval(ctx context.Context, subSID, subToken, contentSID string) error {
	payload := map[string]any{
		"name":     "livecart_cart_recovery",
		"category": "MARKETING",
	}
	_, err := m.doJSON(ctx, http.MethodPost, twilioContentBaseURL+"/Content/"+contentSID+"/ApprovalRequests/whatsapp", payload, subSID, subToken)
	if err != nil {
		return fmt.Errorf("submitting template approval: %w", err)
	}
	return nil
}

// TemplateApprovalStatus is the WhatsApp review state of a template.
type TemplateApprovalStatus struct {
	Status string `json:"status"` // unsubmitted | received | pending | approved | rejected
	Reason string `json:"rejection_reason,omitempty"`
}

// GetTemplateApprovalStatus fetches the WhatsApp approval state.
func (m *MasterClient) GetTemplateApprovalStatus(ctx context.Context, subSID, subToken, contentSID string) (*TemplateApprovalStatus, error) {
	body, err := m.doJSON(ctx, http.MethodGet, twilioContentBaseURL+"/Content/"+contentSID+"/ApprovalRequests", nil, subSID, subToken)
	if err != nil {
		return nil, fmt.Errorf("fetching template approval: %w", err)
	}

	var out struct {
		WhatsApp TemplateApprovalStatus `json:"whatsapp"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing approval response: %w", err)
	}
	return &out.WhatsApp, nil
}

// =============================================================================
// HTTP
// =============================================================================

func (m *MasterClient) doForm(ctx context.Context, method, endpoint string, form url.Values, sid, token string) ([]byte, error) {
	return m.do(ctx, method, endpoint, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", sid, token)
}

func (m *MasterClient) doJSON(ctx context.Context, method, endpoint string, payload any, sid, token string) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshaling payload: %w", err)
		}
		reader = strings.NewReader(string(raw))
	}
	return m.do(ctx, method, endpoint, reader, "application/json", sid, token)
}

func (m *MasterClient) do(ctx context.Context, method, endpoint string, body io.Reader, contentType, sid, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(sid, token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	start := time.Now()
	resp, err := m.httpClient.Do(req)
	m.logger.Debug("twilio master request",
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

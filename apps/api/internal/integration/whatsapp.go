package integration

// WhatsApp (Twilio) service glue — PRD 006.
//
// Sprint 1 scope: template send + test send, webhook signature validation,
// status-callback processing and inbound opt-out. The recovery worker and the
// checkout_reminder fallback (Sprint 3) build on these primitives.

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/communication"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// =============================================================================
// PROVIDER ACCESS
// =============================================================================

// GetCommunicationProvider returns the store's active WhatsApp provider along
// with the integration row (metadata carries sender/template info).
func (s *Service) GetCommunicationProvider(ctx context.Context, storeID string) (providers.CommunicationProvider, *IntegrationRow, error) {
	row, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypeCommunication), string(providers.ProviderTwilioWhatsApp))
	if err != nil {
		return nil, nil, httpx.ErrNotFound("nenhuma integração WhatsApp configurada para esta loja")
	}

	provider, err := s.createProviderFromRow(ctx, row)
	if err != nil {
		return nil, nil, err
	}

	comm, ok := provider.(providers.CommunicationProvider)
	if !ok {
		return nil, nil, httpx.ErrUnprocessable("failed to cast to communication provider")
	}
	return comm, row, nil
}

// =============================================================================
// SENDING
// =============================================================================

// SendWhatsAppTemplateInput describes a business-initiated template send.
type SendWhatsAppTemplateInput struct {
	StoreID           string
	To                string            // E.164 (+5511999999999)
	ContentSid        string            // empty → falls back to the recovery template in metadata
	Variables         map[string]string // keyed "1", "2", ...
	NotificationLogID string            // optional: stamps provider_message_id for status tracking
}

// SendWhatsAppTemplate sends an approved template through the store's Twilio
// subaccount. Status transitions arrive later via webhook.
func (s *Service) SendWhatsAppTemplate(ctx context.Context, input SendWhatsAppTemplateInput) (*providers.MessageResult, error) {
	provider, row, err := s.GetCommunicationProvider(ctx, input.StoreID)
	if err != nil {
		return nil, err
	}

	contentSid := input.ContentSid
	if contentSid == "" {
		contentSid, _ = row.Metadata[communication.MetaContentSIDRecovery].(string)
	}
	if contentSid == "" {
		return nil, httpx.ErrUnprocessable("nenhum template WhatsApp aprovado configurado para esta loja")
	}

	result, err := provider.SendTemplateMessage(ctx, providers.TemplateMessage{
		To:          input.To,
		ContentSid:  contentSid,
		Variables:   input.Variables,
		CallbackURL: twilioWebhookURL(input.StoreID),
	})
	if err != nil {
		return nil, err
	}

	if input.NotificationLogID != "" && result.MessageID != "" {
		if err := s.repo.SetNotificationLogProviderMessageID(ctx, input.NotificationLogID, result.MessageID); err != nil {
			logger.From(ctx, s.logger).Warn("failed to stamp provider message id on notification log",
				zap.String("log_id", input.NotificationLogID),
				zap.String("message_sid", result.MessageID),
				zap.Error(err),
			)
		}
	}

	logger.From(ctx, s.logger).Info("whatsapp template sent",
		zap.String("store_id", input.StoreID),
		zap.String("message_sid", result.MessageID),
		zap.String("status", result.Status),
	)
	return result, nil
}

// SendWhatsAppTest sends the store's recovery template with sample variables
// to a phone the merchant controls (integrations "testar" button).
func (s *Service) SendWhatsAppTest(ctx context.Context, storeID, to string) (*providers.MessageResult, error) {
	frontend := strings.TrimRight(config.FrontendURL.String(), "/")
	return s.SendWhatsAppTemplate(ctx, SendWhatsAppTemplateInput{
		StoreID: storeID,
		To:      to,
		Variables: map[string]string{
			"1": "Cliente Teste",
			"2": "2 itens · R$ 119,80",
			"3": frontend + "/cart/exemplo",
		},
	})
}

// twilioWebhookURL builds the canonical status-callback URL for a store. The
// same URL is used to validate inbound webhook signatures, so both sides must
// derive it identically.
func twilioWebhookURL(storeID string) string {
	return strings.TrimRight(config.WebhookBaseURL.String(), "/") + "/api/webhooks/twilio/" + storeID
}

// =============================================================================
// WEBHOOK: SIGNATURE VALIDATION
// =============================================================================

// ValidateTwilioWebhookSignature checks the X-Twilio-Signature header.
// Twilio signs with the auth token of the account that sent the message (the
// store's subaccount): Base64(HMAC-SHA1(token, url + sorted(k+v)...)).
func (s *Service) ValidateTwilioWebhookSignature(ctx context.Context, storeID string, params map[string]string, signature string) (bool, error) {
	if signature == "" {
		return false, nil
	}

	_, row, err := s.getWhatsAppRow(ctx, storeID)
	if err != nil {
		return false, err
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return false, fmt.Errorf("decrypting credentials: %w", err)
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(twilioWebhookURL(storeID))
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(params[k])
	}

	mac := hmac.New(sha1.New, []byte(creds.APISecret))
	mac.Write([]byte(b.String()))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature)), nil
}

// getWhatsAppRow loads the integration row without instantiating a provider
// (webhook path — cheaper and works even when the sender is still pending).
func (s *Service) getWhatsAppRow(ctx context.Context, storeID string) (providers.ProviderName, *IntegrationRow, error) {
	row, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypeCommunication), string(providers.ProviderTwilioWhatsApp))
	if err != nil {
		return "", nil, httpx.ErrNotFound("nenhuma integração WhatsApp configurada para esta loja")
	}
	return providers.ProviderTwilioWhatsApp, row, nil
}

// =============================================================================
// WEBHOOK: STATUS CALLBACKS + INBOUND
// =============================================================================

// twilioStatusMap converts Twilio message statuses to notification_log
// statuses. Unknown statuses are ignored (returns "").
func twilioStatusMap(twilioStatus string) string {
	switch twilioStatus {
	case "sent":
		return "sent"
	case "delivered":
		return "delivered"
	case "read":
		return "read"
	case "failed", "undelivered":
		return "failed"
	default: // queued, accepted, sending — intermediate, not worth a write
		return ""
	}
}

// ProcessTwilioStatusCallback updates the notification log row correlated by
// MessageSid. Logs for untracked messages (e.g. test sends) are ignored.
func (s *Service) ProcessTwilioStatusCallback(ctx context.Context, storeID, messageSid, twilioStatus, errorCode string) error {
	status := twilioStatusMap(twilioStatus)
	if status == "" || messageSid == "" {
		return nil
	}

	errMsg := ""
	if errorCode != "" && errorCode != "0" {
		errMsg = "twilio error code " + errorCode
	}

	found, err := s.repo.UpdateNotificationLogStatusByProviderMessageID(ctx, messageSid, status, errMsg)
	if err != nil {
		return fmt.Errorf("updating notification log by message sid: %w", err)
	}
	if !found {
		logger.From(ctx, s.logger).Debug("twilio status callback for untracked message",
			zap.String("message_sid", messageSid),
			zap.String("status", twilioStatus),
		)
	}
	return nil
}

// whatsappOptOutWords — inbound replies that opt the customer out (LGPD).
var whatsappOptOutWords = map[string]bool{
	"sair": true, "parar": true, "stop": true, "cancelar": true, "descadastrar": true,
}

// ProcessTwilioInbound handles customer replies. Sprint 1 only implements
// opt-out; other replies are ignored (the merchant sees them on their phone —
// the sender is their own number).
func (s *Service) ProcessTwilioInbound(ctx context.Context, storeID, from, body string) error {
	word := strings.ToLower(strings.TrimSpace(body))
	if !whatsappOptOutWords[word] {
		return nil
	}

	phone := strings.TrimPrefix(from, "whatsapp:")
	rows, err := s.repo.SetCustomerWhatsAppOptOutByPhone(ctx, storeID, phone, true)
	if err != nil {
		return fmt.Errorf("setting whatsapp opt-out: %w", err)
	}

	logger.From(ctx, s.logger).Info("whatsapp opt-out processed",
		zap.String("phone", phone),
		zap.Int64("customers_updated", rows),
	)
	return nil
}

// =============================================================================
// REPOSITORY WRAPPERS
// =============================================================================

// SetNotificationLogProviderMessageID stamps the Twilio MessageSid on a log row.
func (r *Repository) SetNotificationLogProviderMessageID(ctx context.Context, logID, messageSid string) error {
	id, err := parseUUID(logID)
	if err != nil {
		return err
	}
	return r.queries.SetNotificationLogProviderMessageID(ctx, sqlc.SetNotificationLogProviderMessageIDParams{
		ID:                id,
		ProviderMessageID: pgtype.Text{String: messageSid, Valid: true},
	})
}

// UpdateNotificationLogStatusByProviderMessageID applies a status callback.
// Returns false when no log row is tracked for the given MessageSid.
func (r *Repository) UpdateNotificationLogStatusByProviderMessageID(ctx context.Context, messageSid, status, errorMsg string) (bool, error) {
	_, err := r.queries.UpdateNotificationLogByProviderMessageID(ctx, sqlc.UpdateNotificationLogByProviderMessageIDParams{
		ProviderMessageID: pgtype.Text{String: messageSid, Valid: true},
		Status:            status,
		ErrorMessage:      pgtype.Text{String: errorMsg, Valid: errorMsg != ""},
		SentAt:            pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SetCustomerWhatsAppOptOutByPhone flags customers matching store+phone.
func (r *Repository) SetCustomerWhatsAppOptOutByPhone(ctx context.Context, storeID, phone string, optedOut bool) (int64, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return 0, err
	}
	return r.queries.SetCustomerWhatsAppOptOutByPhone(ctx, sqlc.SetCustomerWhatsAppOptOutByPhoneParams{
		StoreID:          sid,
		Phone:            pgtype.Text{String: phone, Valid: true},
		WhatsappOptedOut: optedOut,
	})
}

// =============================================================================
// HTTP HANDLER
// =============================================================================

// SendWhatsAppTestRequest is the body for the WhatsApp test-message endpoint.
type SendWhatsAppTestRequest struct {
	To string `json:"to" validate:"required,e164"`
}

// SendWhatsAppTestMessage sends the store's recovery template with sample
// variables to a number the merchant controls.
// @Summary Send WhatsApp test message
// @Description Sends the approved recovery template with sample data to the given number
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body SendWhatsAppTestRequest true "Destination"
// @Success 200 {object} httpx.Envelope
// @Failure 404 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/integrations/whatsapp/test-message [post]
// @Security BearerAuth
func (h *Handler) SendWhatsAppTestMessage(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req SendWhatsAppTestRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	result, err := h.service.SendWhatsAppTest(c.Context(), storeID, req.To)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, fiber.Map{
		"messageId": result.MessageID,
		"status":    result.Status,
	})
}

// =============================================================================
// ONBOARDING (Sprint 2)
// =============================================================================

// twilioMaster builds the master-account client from config.
func (s *Service) twilioMaster() (*communication.MasterClient, error) {
	return communication.NewMasterClient(config.TwilioAccountSID.String(), config.TwilioAuthToken.String(), s.logger)
}

// ConnectWhatsAppInput starts (or resumes) WhatsApp onboarding for a store.
type ConnectWhatsAppInput struct {
	StoreID     string
	PhoneE164   string // merchant's WhatsApp number
	WABAID      string // from Meta embedded signup (optional until ISV approval)
	DisplayName string // business name shown on WhatsApp
}

// WhatsAppStatusResponse is the FE-facing onboarding/health state.
type WhatsAppStatusResponse struct {
	IntegrationID  string `json:"integrationId"`
	Status         string `json:"status"` // integration lifecycle: pending_auth | active | error
	PhoneNumber    string `json:"phoneNumber"`
	SenderStatus   string `json:"senderStatus"`   // NOT_REGISTERED | PENDING_VERIFICATION | ONLINE | ...
	QualityRating  string `json:"qualityRating"`  // HIGH | MEDIUM | LOW | UNKNOWN
	TemplateStatus string `json:"templateStatus"` // missing | pending | approved | rejected
	TemplateReason string `json:"templateReason,omitempty"`
}

// ConnectWhatsApp provisions the store's subaccount, registers the merchant
// number as a WhatsApp sender (OTP flows to their phone) and submits the
// default recovery template for approval. Safe to call again to resume a
// partially-completed onboarding.
func (s *Service) ConnectWhatsApp(ctx context.Context, input ConnectWhatsAppInput) (*WhatsAppStatusResponse, error) {
	master, err := s.twilioMaster()
	if err != nil {
		return nil, httpx.ErrUnprocessable("integração WhatsApp indisponível: credenciais Twilio não configuradas")
	}

	// Resume path: reuse the existing subaccount/integration when present.
	row, _ := s.repo.GetByProvider(ctx, input.StoreID, string(providers.ProviderTypeCommunication), string(providers.ProviderTwilioWhatsApp))

	var subSID, subToken string
	if row == nil {
		sub, err := master.CreateSubaccount(ctx, "livecart-store-"+input.StoreID)
		if err != nil {
			return nil, fmt.Errorf("creating twilio subaccount: %w", err)
		}
		subSID, subToken = sub.SID, sub.AuthToken

		out, err := s.Create(ctx, CreateIntegrationInput{
			StoreID:  input.StoreID,
			Type:     string(providers.ProviderTypeCommunication),
			Provider: string(providers.ProviderTwilioWhatsApp),
			Credentials: &providers.Credentials{
				APIKey:    subSID,
				APISecret: subToken,
			},
			Metadata: map[string]any{
				communication.MetaPhoneNumber: input.PhoneE164,
				communication.MetaWABAID:      input.WABAID,
				"display_name":                input.DisplayName,
			},
		})
		if err != nil {
			return nil, err
		}
		row, err = s.repo.GetByID(ctx, out.ID, input.StoreID)
		if err != nil {
			return nil, err
		}
	} else {
		creds, err := s.decryptCredentials(row.Credentials)
		if err != nil {
			return nil, fmt.Errorf("decrypting credentials: %w", err)
		}
		subSID, subToken = creds.APIKey, creds.APISecret
	}

	metadata := row.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[communication.MetaPhoneNumber] = input.PhoneE164
	if input.WABAID != "" {
		metadata[communication.MetaWABAID] = input.WABAID
	}
	if input.DisplayName != "" {
		metadata["display_name"] = input.DisplayName
	}

	// Sender registration (best effort — fails cleanly while the ISV/WABA
	// prerequisites are still pending; calling connect again resumes here).
	if _, ok := metadata[communication.MetaSenderSID].(string); !ok {
		wabaID, _ := metadata[communication.MetaWABAID].(string)
		displayName, _ := metadata["display_name"].(string)
		sender, err := master.RegisterSender(ctx, communication.RegisterSenderInput{
			SubaccountSID:   subSID,
			SubaccountToken: subToken,
			PhoneE164:       input.PhoneE164,
			WABAID:          wabaID,
			DisplayName:     displayName,
			CallbackURL:     twilioWebhookURL(input.StoreID),
		})
		if err != nil {
			logger.From(ctx, s.logger).Warn("whatsapp sender registration pending",
				zap.Error(err),
			)
			metadata["sender_error"] = err.Error()
		} else {
			metadata[communication.MetaSenderSID] = sender.SID
			metadata["sender_status"] = sender.Status
			delete(metadata, "sender_error")
		}
	}

	// Default recovery template (best effort, resumable).
	if _, ok := metadata[communication.MetaContentSIDRecovery].(string); !ok {
		displayName, _ := metadata["display_name"].(string)
		if displayName == "" {
			displayName = "sua loja"
		}
		tpl, err := master.CreateRecoveryTemplate(ctx, subSID, subToken, displayName)
		if err != nil {
			logger.From(ctx, s.logger).Warn("whatsapp recovery template creation pending",
				zap.Error(err),
			)
			metadata["template_error"] = err.Error()
		} else {
			metadata[communication.MetaContentSIDRecovery] = tpl.SID
			delete(metadata, "template_error")
			if err := master.SubmitTemplateApproval(ctx, subSID, subToken, tpl.SID); err != nil {
				logger.From(ctx, s.logger).Warn("whatsapp template approval submission failed",
					zap.Error(err),
				)
			}
		}
	}

	if err := s.repo.UpdateMetadata(ctx, row.ID, metadata); err != nil {
		return nil, fmt.Errorf("persisting onboarding metadata: %w", err)
	}
	row.Metadata = metadata

	return s.buildWhatsAppStatus(ctx, row, subSID, subToken)
}

// VerifyWhatsAppSender submits the OTP the merchant received. On ONLINE the
// integration flips to active.
func (s *Service) VerifyWhatsAppSender(ctx context.Context, storeID, code string) (*WhatsAppStatusResponse, error) {
	_, row, err := s.getWhatsAppRow(ctx, storeID)
	if err != nil {
		return nil, err
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}
	senderSID, _ := row.Metadata[communication.MetaSenderSID].(string)
	if senderSID == "" {
		return nil, httpx.ErrUnprocessable("registro do número ainda não foi iniciado")
	}

	master, err := s.twilioMaster()
	if err != nil {
		return nil, httpx.ErrUnprocessable("integração WhatsApp indisponível: credenciais Twilio não configuradas")
	}

	sender, err := master.VerifySender(ctx, creds.APIKey, creds.APISecret, senderSID, code)
	if err != nil {
		return nil, err
	}

	metadata := row.Metadata
	metadata["sender_status"] = sender.Status
	if err := s.repo.UpdateMetadata(ctx, row.ID, metadata); err != nil {
		return nil, fmt.Errorf("persisting sender status: %w", err)
	}

	if strings.EqualFold(sender.Status, "ONLINE") {
		if err := s.repo.UpdateStatus(ctx, row.ID, "active"); err != nil {
			return nil, fmt.Errorf("activating integration: %w", err)
		}
		row.Status = "active"
	}
	row.Metadata = metadata

	return s.buildWhatsAppStatus(ctx, row, creds.APIKey, creds.APISecret)
}

// GetWhatsAppStatus returns onboarding/health state for FE polling.
func (s *Service) GetWhatsAppStatus(ctx context.Context, storeID string) (*WhatsAppStatusResponse, error) {
	_, row, err := s.getWhatsAppRow(ctx, storeID)
	if err != nil {
		return nil, err
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}
	return s.buildWhatsAppStatus(ctx, row, creds.APIKey, creds.APISecret)
}

// buildWhatsAppStatus assembles the FE status payload, refreshing sender and
// template state live (and flipping the integration to active when the sender
// comes ONLINE outside the verify flow).
func (s *Service) buildWhatsAppStatus(ctx context.Context, row *IntegrationRow, subSID, subToken string) (*WhatsAppStatusResponse, error) {
	resp := &WhatsAppStatusResponse{
		IntegrationID:  row.ID,
		Status:         row.Status,
		SenderStatus:   "NOT_REGISTERED",
		QualityRating:  "UNKNOWN",
		TemplateStatus: "missing",
	}
	resp.PhoneNumber, _ = row.Metadata[communication.MetaPhoneNumber].(string)

	if senderStatus, ok := row.Metadata["sender_status"].(string); ok && senderStatus != "" {
		resp.SenderStatus = senderStatus
	}

	// Live sender refresh when registered.
	if senderSID, _ := row.Metadata[communication.MetaSenderSID].(string); senderSID != "" {
		if provider, _, err := s.GetCommunicationProvider(ctx, row.StoreID); err == nil {
			if status, err := provider.GetSenderStatus(ctx); err == nil {
				resp.SenderStatus = status.Status
				resp.QualityRating = status.QualityRating
				if status.PhoneNumber != "" {
					resp.PhoneNumber = status.PhoneNumber
				}
				if strings.EqualFold(status.Status, "ONLINE") && row.Status == "pending_auth" {
					if err := s.repo.UpdateStatus(ctx, row.ID, "active"); err == nil {
						resp.Status = "active"
					}
				}
			}
		}
	}

	// Template approval state.
	if contentSID, _ := row.Metadata[communication.MetaContentSIDRecovery].(string); contentSID != "" {
		resp.TemplateStatus = "pending"
		if master, err := s.twilioMaster(); err == nil {
			if approval, err := master.GetTemplateApprovalStatus(ctx, subSID, subToken, contentSID); err == nil && approval.Status != "" {
				resp.TemplateStatus = approval.Status
				resp.TemplateReason = approval.Reason
			}
		}
	}

	return resp, nil
}

// =============================================================================
// ONBOARDING HTTP HANDLERS
// =============================================================================

// ConnectWhatsAppRequest is the body for the connect endpoint.
type ConnectWhatsAppRequest struct {
	PhoneNumber string `json:"phoneNumber" validate:"required,e164"`
	WABAID      string `json:"wabaId,omitempty"`
	DisplayName string `json:"displayName,omitempty" validate:"omitempty,max=100"`
}

// VerifyWhatsAppRequest is the body for the OTP verify endpoint.
type VerifyWhatsAppRequest struct {
	Code string `json:"code" validate:"required,min=4,max=10"`
}

// ConnectWhatsApp starts or resumes WhatsApp onboarding for the store.
// @Summary Connect WhatsApp
// @Description Provisions a Twilio subaccount, registers the merchant number (OTP) and submits the default template
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body ConnectWhatsAppRequest true "Merchant number"
// @Success 200 {object} httpx.Envelope{data=WhatsAppStatusResponse}
// @Router /api/v1/stores/{storeId}/integrations/whatsapp/connect [post]
// @Security BearerAuth
func (h *Handler) ConnectWhatsApp(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req ConnectWhatsAppRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	resp, err := h.service.ConnectWhatsApp(c.Context(), ConnectWhatsAppInput{
		StoreID:     storeID,
		PhoneE164:   req.PhoneNumber,
		WABAID:      req.WABAID,
		DisplayName: req.DisplayName,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
}

// VerifyWhatsApp submits the OTP received on the merchant's phone.
// @Summary Verify WhatsApp sender
// @Description Submits the SMS/voice OTP; on success the integration becomes active
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body VerifyWhatsAppRequest true "OTP code"
// @Success 200 {object} httpx.Envelope{data=WhatsAppStatusResponse}
// @Router /api/v1/stores/{storeId}/integrations/whatsapp/verify [post]
// @Security BearerAuth
func (h *Handler) VerifyWhatsApp(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req VerifyWhatsAppRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	resp, err := h.service.VerifyWhatsAppSender(c.Context(), storeID, req.Code)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
}

// GetWhatsAppStatus returns onboarding/health state for polling.
// @Summary Get WhatsApp status
// @Description Returns sender, quality and template approval state
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope{data=WhatsAppStatusResponse}
// @Router /api/v1/stores/{storeId}/integrations/whatsapp/status [get]
// @Security BearerAuth
func (h *Handler) GetWhatsAppStatus(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	resp, err := h.service.GetWhatsAppStatus(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, resp)
}

// SendCartRecoveryTemplate implements notification.WhatsAppSender: sends the
// store's approved recovery template (reminder fallback uses the same one).
func (s *Service) SendCartRecoveryTemplate(ctx context.Context, storeID, toPhone string, variables map[string]string, logID string) error {
	_, err := s.SendWhatsAppTemplate(ctx, SendWhatsAppTemplateInput{
		StoreID:           storeID,
		To:                toPhone,
		Variables:         variables,
		NotificationLogID: logID,
	})
	return err
}

// =============================================================================
// RECOVERY STATS (Sprint 4)
// =============================================================================

// WhatsAppRecoveryStats is the last-30-days recovery funnel for the store.
type WhatsAppRecoveryStats struct {
	MessagesSent          int32 `json:"messagesSent"`
	CartsRecovered        int32 `json:"cartsRecovered"`
	RevenueRecoveredCents int64 `json:"revenueRecoveredCents"`
}

// GetWhatsAppRecoveryStats returns the recovery funnel (PRD 005 attribution
// window: paid within 48h of the message).
func (s *Service) GetWhatsAppRecoveryStats(ctx context.Context, storeID string) (*WhatsAppRecoveryStats, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	row, err := s.repo.queries.GetWhatsAppRecoveryStats(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("fetching recovery stats: %w", err)
	}
	return &WhatsAppRecoveryStats{
		MessagesSent:          row.MessagesSent,
		CartsRecovered:        row.CartsRecovered,
		RevenueRecoveredCents: row.RevenueRecoveredCents,
	}, nil
}

// GetWhatsAppRecoveryStatsHandler exposes the recovery funnel.
// @Summary Get WhatsApp recovery stats
// @Description Last-30-days recovery funnel: messages, recovered carts, recovered revenue
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope{data=WhatsAppRecoveryStats}
// @Router /api/v1/stores/{storeId}/integrations/whatsapp/recovery-stats [get]
// @Security BearerAuth
func (h *Handler) GetWhatsAppRecoveryStatsHandler(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	stats, err := h.service.GetWhatsAppRecoveryStats(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, stats)
}

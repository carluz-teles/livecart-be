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
			s.logger.Warn("failed to stamp provider message id on notification log",
				zap.String("log_id", input.NotificationLogID),
				zap.String("message_sid", result.MessageID),
				zap.Error(err),
			)
		}
	}

	s.logger.Info("whatsapp template sent",
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
		s.logger.Debug("twilio status callback for untracked message",
			zap.String("store_id", storeID),
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

	s.logger.Info("whatsapp opt-out processed",
		zap.String("store_id", storeID),
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

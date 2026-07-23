package notification

// WhatsApp fallback for the pre-expiration reminder (PRD 006 §3): the
// checkout_reminder keeps going out on Instagram first; when the 24h
// messaging window is closed and the buyer left a phone + consent at
// checkout, the approved WhatsApp template goes out instead.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

// WhatsAppSender sends the approved recovery/reminder template through the
// store's WhatsApp number. Implemented by the integration service (kept as a
// narrow interface here to avoid an import cycle).
type WhatsAppSender interface {
	SendCartRecoveryTemplate(ctx context.Context, storeID, toPhone string, variables map[string]string, logID string) error
}

// SetWhatsAppSender wires the WhatsApp channel (optional — reminder falls
// back only when configured).
func (s *Service) SetWhatsAppSender(w WhatsAppSender) {
	s.whatsappSender = w
}

// tryWhatsAppFallback attempts the reminder via WhatsApp after an IG failure.
// Returns the log ID when a message went out, "" otherwise. Never errors —
// notification failures must not break the caller flow.
func (s *Service) tryWhatsAppFallback(ctx context.Context, input SendInput) string {
	if s.whatsappSender == nil || input.NotificationType != TypeCheckoutReminder || input.CartID == "" {
		return ""
	}

	var cartID pgtype.UUID
	if err := cartID.Scan(input.CartID); err != nil {
		return ""
	}
	cart, err := s.queries.GetCartByID(ctx, cartID)
	if err != nil || !cart.WhatsappConsent || !cart.CustomerPhone.Valid || cart.CustomerPhone.String == "" {
		return ""
	}

	// LGPD: respect SAIR/PARAR replies.
	storeUID, err := parseUUID(input.StoreID)
	if err != nil {
		return ""
	}
	optedOut, err := s.queries.IsCustomerWhatsAppOptedOut(ctx, sqlc.IsCustomerWhatsAppOptedOutParams{
		StoreID: storeUID,
		Phone:   cart.CustomerPhone,
	})
	if err != nil || optedOut {
		return ""
	}

	// Template variables: {{1}} nome, {{2}} resumo, {{3}} link — the same
	// approved recovery template doubles as the reminder for now.
	name := strings.TrimSpace(cart.CustomerName.String)
	if name == "" {
		name = strings.TrimPrefix(input.PlatformHandle, "@")
	}
	if name == "" {
		name = "Cliente"
	}
	if i := strings.IndexByte(name, ' '); i > 0 {
		name = name[:i]
	}
	vars := map[string]string{
		"1": name,
		"2": fmt.Sprintf("%d itens · %s", input.Variables.TotalItens, input.Variables.Total),
		"3": input.Variables.Link,
	}

	logID, err := s.createWhatsAppLog(ctx, input, cart.CustomerPhone.String, vars)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to create whatsapp fallback log", zap.Error(err))
	}

	if err := s.whatsappSender.SendCartRecoveryTemplate(ctx, input.StoreID, cart.CustomerPhone.String, vars, logID); err != nil {
		s.updateLogStatus(ctx, logID, StatusFailed, err.Error())
		logger.From(ctx, s.logger).Warn("whatsapp reminder fallback failed",
			zap.String("store_id", input.StoreID),
			zap.String("cart_id", input.CartID),
			zap.Error(err),
		)
		// Group I: notification.failed (channel=whatsapp) — merged into the unified
		// notification.* vocabulary (was whatsapp.fallback_failed).
		waFailKey := logID
		if waFailKey == "" {
			waFailKey = input.StoreID + ":" + input.CartID
		}
		_ = events.EmitInternal(ctx, s.queries, events.NotificationFailed,
			"notification.failed:whatsapp:"+waFailKey,
			struct {
				StoreID          string `json:"store_id"`
				NotificationType string `json:"notification_type"`
				Channel          string `json:"channel"`
				Recipient        string `json:"recipient"`
				CartID           string `json:"cart_id,omitempty"`
				LogID            string `json:"log_id,omitempty"`
				Error            string `json:"error"`
			}{input.StoreID, string(input.NotificationType), string(ChannelWhatsApp), cart.CustomerPhone.String, input.CartID, logID, err.Error()})
		return ""
	}

	s.updateLogStatus(ctx, logID, StatusSent, "")
	logger.From(ctx, s.logger).Info("reminder delivered via whatsapp fallback",
		zap.String("store_id", input.StoreID),
		zap.String("cart_id", input.CartID),
	)
	// Group I: notification.sent (channel=whatsapp) — merged into the unified
	// notification.* vocabulary (was whatsapp.fallback_sent).
	waSentKey := logID
	if waSentKey == "" {
		waSentKey = input.StoreID + ":" + input.CartID
	}
	_ = events.EmitInternal(ctx, s.queries, events.NotificationSent,
		"notification.sent:whatsapp:"+waSentKey,
		struct {
			StoreID          string `json:"store_id"`
			NotificationType string `json:"notification_type"`
			Channel          string `json:"channel"`
			Recipient        string `json:"recipient"`
			CartID           string `json:"cart_id,omitempty"`
			LogID            string `json:"log_id,omitempty"`
		}{input.StoreID, string(input.NotificationType), string(ChannelWhatsApp), cart.CustomerPhone.String, input.CartID, logID})
	return logID
}

// createWhatsAppLog mirrors createLog with the whatsapp channel and the phone
// as the platform user key.
func (s *Service) createWhatsAppLog(ctx context.Context, input SendInput, phone string, vars map[string]string) (string, error) {
	storeUID, err := parseUUID(input.StoreID)
	if err != nil {
		return "", err
	}
	var eventID, cartID pgtype.UUID
	if input.EventID != "" {
		eventID, _ = parseUUID(input.EventID)
	}
	if input.CartID != "" {
		cartID, _ = parseUUID(input.CartID)
	}

	log, err := s.queries.CreateNotificationLog(ctx, sqlc.CreateNotificationLogParams{
		StoreID:          storeUID,
		EventID:          eventID,
		CartID:           cartID,
		PlatformUserID:   phone,
		PlatformHandle:   pgtype.Text{String: input.PlatformHandle, Valid: input.PlatformHandle != ""},
		NotificationType: string(input.NotificationType),
		Channel:          string(ChannelWhatsApp),
		Status:           string(StatusPending),
		MessageText:      pgtype.Text{String: fmt.Sprintf("fallback whatsapp vars=%v", vars), Valid: true},
	})
	if err != nil {
		return "", err
	}
	return log.ID.String(), nil
}

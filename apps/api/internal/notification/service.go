package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
)

// DMSender is an interface for sending direct messages.
type DMSender interface {
	SendInstagramDM(ctx context.Context, storeID, recipientID, text string) error
	ReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error
}

// Service handles notification logic including templates, cooldowns, and logging.
type Service struct {
	queries  *sqlc.Queries
	dmSender DMSender
	logger   *zap.Logger
}

// NewService creates a new notification service.
func NewService(queries *sqlc.Queries, dmSender DMSender, logger *zap.Logger) *Service {
	return &Service{
		queries:  queries,
		dmSender: dmSender,
		logger:   logger.Named("notification"),
	}
}

// GetSettings retrieves notification settings (templates) for a store.
func (s *Service) GetSettings(ctx context.Context, storeID string) (*Settings, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	settingsJSON, err := s.queries.GetStoreNotificationSettings(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("getting notification settings: %w", err)
	}

	// If settings are null, return defaults
	if settingsJSON == nil || len(settingsJSON) == 0 {
		settings := DefaultSettings()
		return &settings, nil
	}

	var settings Settings
	if err := json.Unmarshal(settingsJSON, &settings); err != nil {
		s.logger.Warn("failed to parse notification settings, using defaults",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		defaults := DefaultSettings()
		return &defaults, nil
	}

	return &settings, nil
}

// GetCartMessageSettings retrieves cart message settings (triggers) for a store.
func (s *Service) GetCartMessageSettings(ctx context.Context, storeID string) (*CartMessageSettings, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := s.queries.GetStoreCartMessageSettings(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("getting cart message settings: %w", err)
	}

	return &CartMessageSettings{
		RealTimeCart:              row.CartRealTime,
		SendExpirationReminder:    row.CartSendExpirationReminder,
		ExpirationReminderMinutes: int(row.CartExpirationReminderMinutes),
	}, nil
}

// UpdateSettings updates notification settings for a store.
func (s *Service) UpdateSettings(ctx context.Context, storeID string, settings Settings) error {
	uid, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	return s.queries.UpdateStoreNotificationSettings(ctx, sqlc.UpdateStoreNotificationSettingsParams{
		ID:                   uid,
		NotificationSettings: settingsJSON,
	})
}

// Send sends a notification based on type and store settings.
func (s *Service) Send(ctx context.Context, input SendInput) (*SendResult, error) {
	// Get template settings
	settings, err := s.GetSettings(ctx, input.StoreID)
	if err != nil {
		return nil, fmt.Errorf("getting settings: %w", err)
	}

	// Get template settings for this notification type
	templateSettings := s.getTemplateSettings(settings, input.NotificationType)
	if templateSettings == nil || !templateSettings.Enabled {
		s.logger.Debug("notification type disabled",
			zap.String("store_id", input.StoreID),
			zap.String("type", string(input.NotificationType)),
		)
		return &SendResult{
			Status: StatusSkipped,
		}, nil
	}

	// Render the message
	message := RenderTemplate(templateSettings.Template, input.Variables)

	// Truncate if too long
	if len(message) > MaxMessageBytes {
		message = TruncateMessage(message, MaxMessageBytes)
	}

	// Create log entry as pending
	logID, err := s.createLog(ctx, input, StatusPending, message, nil)
	if err != nil {
		s.logger.Warn("failed to create notification log", zap.Error(err))
	}

	// Try to reply to comment first (no 24h window restriction), then fallback to DM
	var sendErr error
	if input.PlatformCommentID != "" {
		s.logger.Debug("trying comment reply first",
			zap.String("comment_id", input.PlatformCommentID),
		)
		sendErr = s.dmSender.ReplyToInstagramComment(ctx, input.StoreID, input.PlatformCommentID, message)
		if sendErr != nil {
			s.logger.Warn("comment reply failed, falling back to DM",
				zap.String("comment_id", input.PlatformCommentID),
				zap.Error(sendErr),
			)
			// Fallback to DM
			sendErr = s.dmSender.SendInstagramDM(ctx, input.StoreID, input.PlatformUserID, message)
		}
	} else {
		// No comment ID, send DM directly
		sendErr = s.dmSender.SendInstagramDM(ctx, input.StoreID, input.PlatformUserID, message)
	}

	if sendErr != nil {
		// Update log as failed
		s.updateLogStatus(ctx, logID, StatusFailed, sendErr.Error())

		s.logger.Warn("failed to send notification",
			zap.String("store_id", input.StoreID),
			zap.String("platform_user_id", input.PlatformUserID),
			zap.String("type", string(input.NotificationType)),
			zap.Error(sendErr),
		)

		return &SendResult{
			LogID:       logID,
			Status:      StatusFailed,
			MessageText: message,
			Error:       sendErr,
		}, nil // Don't return error - notification failures shouldn't break the flow
	}

	// Update log as sent
	s.updateLogStatus(ctx, logID, StatusSent, "")

	s.logger.Info("notification sent",
		zap.String("store_id", input.StoreID),
		zap.String("platform_user_id", input.PlatformUserID),
		zap.String("type", string(input.NotificationType)),
		zap.Int("message_length", len(message)),
	)

	return &SendResult{
		LogID:       logID,
		Status:      StatusSent,
		MessageText: message,
	}, nil
}

// ShouldNotify checks if a notification should be sent based on type and cart state.
// It uses cart_settings for triggers (when to send) and notification_settings for templates (what to send).
func (s *Service) ShouldNotify(ctx context.Context, storeID string, notifType NotificationType, isNewCart bool) (bool, error) {
	// Get template settings (to check if template is enabled)
	templateSettings, err := s.GetSettings(ctx, storeID)
	if err != nil {
		return false, err
	}

	// Get cart message settings (triggers)
	cartSettings, err := s.GetCartMessageSettings(ctx, storeID)
	if err != nil {
		return false, err
	}

	switch notifType {
	case TypeCheckoutImmediate:
		// Template must be enabled and RealTimeCart must be on
		if templateSettings.CheckoutImmediate == nil || !templateSettings.CheckoutImmediate.Enabled {
			return false, nil
		}
		// Only send if real-time cart is enabled
		return cartSettings.RealTimeCart && isNewCart, nil

	case TypeItemAdded:
		if templateSettings.ItemAdded == nil {
			return false, nil
		}
		// Only send for existing carts when real-time cart is enabled
		return templateSettings.ItemAdded.Enabled && !isNewCart && cartSettings.RealTimeCart, nil

	case TypeCheckoutReminder:
		if templateSettings.CheckoutReminder == nil {
			return false, nil
		}
		// Use cart_settings trigger for expiration reminder
		return templateSettings.CheckoutReminder.Enabled && cartSettings.SendExpirationReminder, nil

	default:
		return false, nil
	}
}

// PreviewTemplate renders a template with sample data for preview.
func (s *Service) PreviewTemplate(template string) (string, int, error) {
	rendered := RenderTemplate(template, SampleVariables())
	byteLen := len(rendered)

	if byteLen > MaxMessageBytes {
		return rendered, byteLen, fmt.Errorf("mensagem muito longa: %d bytes (máximo: %d)", byteLen, MaxMessageBytes)
	}

	return rendered, byteLen, nil
}

// Helper methods

func (s *Service) getTemplateSettings(settings *Settings, notifType NotificationType) *TemplateSettings {
	if settings == nil {
		return nil
	}

	switch notifType {
	case TypeCheckoutImmediate:
		return settings.CheckoutImmediate
	case TypeItemAdded:
		return settings.ItemAdded
	case TypeCheckoutReminder:
		return settings.CheckoutReminder
	default:
		return nil
	}
}

func (s *Service) createLog(ctx context.Context, input SendInput, status NotificationStatus, message string, errMsg *string) (string, error) {
	storeUID, err := parseUUID(input.StoreID)
	if err != nil {
		return "", err
	}

	var eventID pgtype.UUID
	if input.EventID != "" {
		eventID, _ = parseUUID(input.EventID)
	}

	var cartID pgtype.UUID
	if input.CartID != "" {
		cartID, _ = parseUUID(input.CartID)
	}

	var handle pgtype.Text
	if input.PlatformHandle != "" {
		handle = pgtype.Text{String: input.PlatformHandle, Valid: true}
	}

	var msgText pgtype.Text
	if message != "" {
		msgText = pgtype.Text{String: message, Valid: true}
	}

	log, err := s.queries.CreateNotificationLog(ctx, sqlc.CreateNotificationLogParams{
		StoreID:          storeUID,
		EventID:          eventID,
		CartID:           cartID,
		PlatformUserID:   input.PlatformUserID,
		PlatformHandle:   handle,
		NotificationType: string(input.NotificationType),
		Channel:          string(ChannelInstagramDM),
		Status:           string(status),
		MessageText:      msgText,
	})
	if err != nil {
		return "", err
	}

	return log.ID.String(), nil
}

func (s *Service) updateLogStatus(ctx context.Context, logID string, status NotificationStatus, errMsg string) {
	if logID == "" {
		return
	}

	uid, err := parseUUID(logID)
	if err != nil {
		return
	}

	var sentAt pgtype.Timestamptz
	if status == StatusSent {
		sentAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}

	var errorMessage pgtype.Text
	if errMsg != "" {
		errorMessage = pgtype.Text{String: errMsg, Valid: true}
	}

	_ = s.queries.UpdateNotificationLogStatus(ctx, sqlc.UpdateNotificationLogStatusParams{
		ID:           uid,
		Status:       string(status),
		SentAt:       sentAt,
		ErrorMessage: errorMessage,
	})
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, fmt.Errorf("invalid UUID: %s", s)
	}
	return uid, nil
}

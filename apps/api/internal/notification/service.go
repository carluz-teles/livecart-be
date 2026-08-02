package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

// DMSender is an interface for sending direct messages.
type DMSender interface {
	SendInstagramDM(ctx context.Context, storeID, recipientID, text string) error
	ReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error
}

// Service handles notification logic including templates, cooldowns, and logging.
type Service struct {
	queries        *sqlc.Queries
	dmSender       DMSender
	emailSender    EmailSender
	whatsappSender WhatsAppSender // optional — reminder fallback (PRD 006)
	logger         *zap.Logger
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
		logger.From(ctx, s.logger).Warn("failed to parse notification settings, using defaults",
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

// mergeSettings overlays onto current only the sections incoming actually
// carries. A nil pointer in incoming means "not provided", so the stored value
// survives.
//
// This exists because UpdateStoreNotificationSettings replaces the whole JSONB
// column. Callers build a Settings from an inbound DTO that does not have a
// field for every section — UpdateSettingsRequest has no waitlist_notified and
// no cart_recovery at all, and the communications tab does not send
// payment_cancelled / payment_refunded. Marshaling that partial struct straight
// into the column dropped those keys on every save, silently resetting the
// merchant's waitlist and cart-recovery configuration.
func mergeSettings(current, incoming Settings) Settings {
	merged := current

	if incoming.CheckoutImmediate != nil {
		merged.CheckoutImmediate = incoming.CheckoutImmediate
	}
	if incoming.ItemAdded != nil {
		merged.ItemAdded = incoming.ItemAdded
	}
	if incoming.CheckoutReminder != nil {
		merged.CheckoutReminder = incoming.CheckoutReminder
	}
	if incoming.WaitlistNotified != nil {
		merged.WaitlistNotified = incoming.WaitlistNotified
	}
	if incoming.OutOfWindowScheduled != nil {
		merged.OutOfWindowScheduled = incoming.OutOfWindowScheduled
	}
	if incoming.OutOfWindowSessionEnded != nil {
		merged.OutOfWindowSessionEnded = incoming.OutOfWindowSessionEnded
	}
	if incoming.OutOfWindowEventEnded != nil {
		merged.OutOfWindowEventEnded = incoming.OutOfWindowEventEnded
	}
	if incoming.EventDeadlineStarted != nil {
		merged.EventDeadlineStarted = incoming.EventDeadlineStarted
	}
	if incoming.WaitlistUnfulfilled != nil {
		merged.WaitlistUnfulfilled = incoming.WaitlistUnfulfilled
	}
	if incoming.PaymentConfirmed != nil {
		merged.PaymentConfirmed = incoming.PaymentConfirmed
	}
	if incoming.Shipped != nil {
		merged.Shipped = incoming.Shipped
	}
	if incoming.Delivered != nil {
		merged.Delivered = incoming.Delivered
	}
	if incoming.PaymentCancelled != nil {
		merged.PaymentCancelled = incoming.PaymentCancelled
	}
	if incoming.PaymentRefunded != nil {
		merged.PaymentRefunded = incoming.PaymentRefunded
	}
	if incoming.CartRecovery != nil {
		merged.CartRecovery = incoming.CartRecovery
	}

	return merged
}

// UpdateSettings updates notification settings for a store.
//
// The write is a partial merge, not a replace: sections absent from settings
// keep whatever is stored today. See mergeSettings.
func (s *Service) UpdateSettings(ctx context.Context, storeID string, settings Settings) error {
	uid, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	current, err := s.GetSettings(ctx, storeID)
	if err != nil {
		return fmt.Errorf("loading current settings before merge: %w", err)
	}

	settingsJSON, err := json.Marshal(mergeSettings(*current, settings))
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	return s.queries.UpdateStoreNotificationSettings(ctx, sqlc.UpdateStoreNotificationSettingsParams{
		ID:                   uid,
		NotificationSettings: settingsJSON,
	})
}

// UndeliveredEntry é uma pessoa que não pôde ser avisada nesta campanha —
// não uma tentativa. Ver ListUndeliveredByEvent: a query colapsa por
// comprador, porque o lojista precisa de "quem eu chamo na mão", não do log.
type UndeliveredEntry struct {
	PlatformUserID   string              `json:"platformUserId"`
	PlatformHandle   string              `json:"platformHandle"`
	NotificationType NotificationType    `json:"notificationType"`
	Reason           UndeliverableReason `json:"reason"`
	// ReasonText é a frase pronta para o painel. Vem do domínio para que a
	// lista, o e-mail de aviso e qualquer outra superfície digam a mesma coisa.
	ReasonText     string    `json:"reasonText"`
	CartID         string    `json:"cartId,omitempty"`
	CartToken      string    `json:"cartToken,omitempty"`
	CartTotalCents int64     `json:"cartTotalCents"`
	CartTotalItems int       `json:"cartTotalItems"`
	CreatedAt      time.Time `json:"createdAt"`
}

// ListUndelivered devolve os compradores não avisados de um evento (RN-38).
func (s *Service) ListUndelivered(ctx context.Context, storeID, eventID string) ([]UndeliveredEntry, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := s.queries.ListUndeliveredByEvent(ctx, sqlc.ListUndeliveredByEventParams{
		StoreID: storeUID,
		EventID: eventUID,
	})
	if err != nil {
		return nil, fmt.Errorf("listing undelivered notifications: %w", err)
	}

	out := make([]UndeliveredEntry, 0, len(rows))
	for _, r := range rows {
		reason := UndeliverableReason(r.UndeliveredReason.String)
		entry := UndeliveredEntry{
			PlatformUserID:   r.PlatformUserID,
			PlatformHandle:   r.PlatformHandle.String,
			NotificationType: NotificationType(r.NotificationType),
			Reason:           reason,
			ReasonText:       UndeliverableReasonText(reason),
			CartTotalCents:   r.CartTotalCents,
			CartTotalItems:   int(r.CartTotalItems),
			CreatedAt:        r.CreatedAt.Time,
		}
		if r.CartID.Valid {
			entry.CartID = r.CartID.String()
		}
		if r.CartToken.Valid {
			entry.CartToken = r.CartToken.String
		}
		out = append(out, entry)
	}
	return out, nil
}

// Send sends a notification based on type and store settings.
func (s *Service) Send(ctx context.Context, input SendInput) (*SendResult, error) {
	// Group I (observability): a DM/notification send was requested. Best-effort;
	// dedup on store+recipient+type+cart so retries of the same send collapse.
	_ = events.EmitInternal(ctx, s.queries, events.NotificationRequested,
		"notification.requested:"+input.StoreID+":"+input.PlatformUserID+":"+string(input.NotificationType)+":"+input.CartID,
		struct {
			StoreID          string `json:"store_id"`
			NotificationType string `json:"notification_type"`
			Channel          string `json:"channel"`
			Recipient        string `json:"recipient"`
			CartID           string `json:"cart_id,omitempty"`
		}{input.StoreID, string(input.NotificationType), string(ChannelInstagramDM), input.PlatformUserID, input.CartID})

	// Get template settings
	settings, err := s.GetSettings(ctx, input.StoreID)
	if err != nil {
		return nil, fmt.Errorf("getting settings: %w", err)
	}

	// Get template settings for this notification type
	templateSettings := s.getTemplateSettings(settings, input.NotificationType)
	if templateSettings == nil || !templateSettings.Enabled {
		logger.From(ctx, s.logger).Debug("notification type disabled",
			zap.String("store_id", input.StoreID),
			zap.String("type", string(input.NotificationType)),
		)
		// Group I: skipped because the template is disabled in store settings.
		_ = events.EmitInternal(ctx, s.queries, events.NotificationSkipped,
			"notification.skipped:"+input.StoreID+":"+input.PlatformUserID+":"+string(input.NotificationType)+":"+input.CartID,
			struct {
				StoreID          string `json:"store_id"`
				NotificationType string `json:"notification_type"`
				Channel          string `json:"channel"`
				Recipient        string `json:"recipient"`
				CartID           string `json:"cart_id,omitempty"`
				Reason           string `json:"reason"`
			}{input.StoreID, string(input.NotificationType), string(ChannelInstagramDM), input.PlatformUserID, input.CartID, "template_disabled"})
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
		logger.From(ctx, s.logger).Warn("failed to create notification log", zap.Error(err))
	}

	// RN-38 — a porta pode já estar fechada ANTES da tentativa.
	//
	// O private reply do Instagram vale 7 dias a contar do comentário. Numa
	// campanha de uma semana, a mensagem de maior conversão (o prazo começou)
	// dispara dias depois do último comentário do comprador, e para parte deles
	// a janela já venceu. Um comentário sozinho NÃO abre a janela de 24h de DM
	// (erro 2534022), então nesse estado não há caminho de entrega nenhum.
	//
	// Não tentar é deliberado: o que a regra proíbe é a ILUSÃO de entrega, e um
	// 'failed' com o erro cru do Graph não diz ao lojista que ele precisa
	// chamar essa pessoa na mão. A linha fica registrada com o motivo e a
	// pessoa aparece na lista.
	//
	// Só vale para o canal de comentário: DirectOnly é a entrega por DM de
	// story, que existe justamente porque o comprador mandou mensagem — ali a
	// janela é outra e a tentativa é legítima.
	if !input.DirectOnly && !input.CommentCreatedAt.IsZero() &&
		time.Since(input.CommentCreatedAt) > PrivateReplyWindow {
		s.markUndelivered(ctx, logID, ReasonCommentWindowExpired)
		logger.From(ctx, s.logger).Info("notification not delivered: private reply window closed",
			zap.String("store_id", input.StoreID),
			zap.String("platform_user_id", input.PlatformUserID),
			zap.String("type", string(input.NotificationType)),
			zap.Time("comment_created_at", input.CommentCreatedAt),
		)
		return &SendResult{
			LogID:       logID,
			Status:      StatusUndelivered,
			MessageText: message,
			Reason:      ReasonCommentWindowExpired,
		}, nil
	}

	// Try to reply to comment first (no 24h window restriction), then fallback to DM
	var sendErr error
	if input.PlatformCommentID != "" {
		logger.From(ctx, s.logger).Debug("trying comment reply first",
			zap.String("comment_id", input.PlatformCommentID),
		)
		sendErr = s.dmSender.ReplyToInstagramComment(ctx, input.StoreID, input.PlatformCommentID, message)
		if sendErr != nil {
			logger.From(ctx, s.logger).Warn("comment reply failed, falling back to DM",
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

		// RN-38 — o status continua 'failed' (foi tentativa real, e é isso que
		// distingue "o Instagram recusou" de "nem tentamos"), mas a linha ganha
		// motivo legível para aparecer na lista do lojista. Sem isso, uma
		// recusa do Graph some num error_message que ninguém lê.
		reason := ReasonInstagramRejected
		if input.PlatformCommentID == "" && !input.DirectOnly {
			// Não havia comentário para responder e o DM direto não passou: é
			// o caso "comprou por story/DM" ou "comentário apagado", que tem
			// texto próprio para o lojista.
			reason = ReasonNoEligibleComment
		}
		s.stampUndeliveredReason(ctx, logID, reason)

		logger.From(ctx, s.logger).Warn("failed to send notification",
			zap.String("store_id", input.StoreID),
			zap.String("platform_user_id", input.PlatformUserID),
			zap.String("type", string(input.NotificationType)),
			zap.Error(sendErr),
		)

		// Group I: the Instagram send failed. Dedup on the log id when we have
		// one (unique per attempt), else on store+recipient+type+cart.
		failKey := logID
		if failKey == "" {
			failKey = input.StoreID + ":" + input.PlatformUserID + ":" + string(input.NotificationType) + ":" + input.CartID
		}
		_ = events.EmitInternal(ctx, s.queries, events.NotificationFailed,
			"notification.failed:"+failKey,
			struct {
				StoreID          string `json:"store_id"`
				NotificationType string `json:"notification_type"`
				Channel          string `json:"channel"`
				Recipient        string `json:"recipient"`
				CartID           string `json:"cart_id,omitempty"`
				LogID            string `json:"log_id,omitempty"`
				Error            string `json:"error"`
			}{input.StoreID, string(input.NotificationType), string(ChannelInstagramDM), input.PlatformUserID, input.CartID, logID, sendErr.Error()})

		// PRD 006: the checkout reminder falls back to WhatsApp when the IG
		// window is closed and the buyer left phone + consent at checkout.
		if waLogID := s.tryWhatsAppFallback(ctx, input); waLogID != "" {
			return &SendResult{
				LogID:       waLogID,
				Status:      StatusSent,
				MessageText: message,
			}, nil
		}

		return &SendResult{
			LogID:       logID,
			Status:      StatusFailed,
			MessageText: message,
			Error:       sendErr,
			Reason:      reason,
		}, nil // Don't return error - notification failures shouldn't break the flow
	}

	// Update log as sent
	s.updateLogStatus(ctx, logID, StatusSent, "")

	logger.From(ctx, s.logger).Info("notification sent",
		zap.String("store_id", input.StoreID),
		zap.String("platform_user_id", input.PlatformUserID),
		zap.String("type", string(input.NotificationType)),
		zap.Int("message_length", len(message)),
	)

	// Group I: the Instagram send succeeded.
	sentKey := logID
	if sentKey == "" {
		sentKey = input.StoreID + ":" + input.PlatformUserID + ":" + string(input.NotificationType) + ":" + input.CartID
	}
	_ = events.EmitInternal(ctx, s.queries, events.NotificationSent,
		"notification.sent:"+sentKey,
		struct {
			StoreID          string `json:"store_id"`
			NotificationType string `json:"notification_type"`
			Channel          string `json:"channel"`
			Recipient        string `json:"recipient"`
			CartID           string `json:"cart_id,omitempty"`
			LogID            string `json:"log_id,omitempty"`
		}{input.StoreID, string(input.NotificationType), string(ChannelInstagramDM), input.PlatformUserID, input.CartID, logID})

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

	case TypeWaitlistNotified,
		TypeOutOfWindowScheduled, TypeOutOfWindowSessionEnded, TypeOutOfWindowEventEnded,
		TypeEventDeadlineStarted, TypeWaitlistUnfulfilled:
		// Sem gatilho em cart_settings: estes momentos não são "mensagem de
		// marketing em tempo real", são resposta a um fato do ciclo de vida
		// (comentou fora da janela, a campanha fechou, o item não liberou). O
		// lojista só opta por fora desligando o próprio template.
		//
		// Amarrá-los a cart_real_time seria pior do que inútil: quem desliga
		// "carrinho em tempo real" quer parar a DM de cada item adicionado, e
		// acabaria silenciando também o aviso de prazo — a mensagem que mais
		// converte — sem nunca ter pedido isso.
		section := s.getTemplateSettings(templateSettings, notifType)
		if section == nil {
			return false, nil
		}
		return section.Enabled, nil

	default:
		return false, nil
	}
}

// SendTestEmail renders the merchant draft (subject + body_html) with sample
// variables and dispatches it through the Resend client. Loads the store row
// for branding (name, logo) so the email looks correct end-to-end. Falls
// back to the default subject when the merchant left it blank.
func (s *Service) SendTestEmail(ctx context.Context, storeID, notifType, subject, bodyHTML, recipientEmail string) error {
	uid, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	store, err := s.queries.GetStoreByID(ctx, uid)
	if err != nil {
		return fmt.Errorf("loading store: %w", err)
	}

	logoURL := ""
	if store.LogoUrl.Valid {
		logoURL = store.LogoUrl.String
	}

	v := SampleVariables()
	renderedSubject := subject
	if renderedSubject == "" {
		renderedSubject = defaultSubjectFor(notifType, v)
	} else {
		renderedSubject = RenderTemplate(renderedSubject, v)
	}
	renderedBody := bodyHTML
	if renderedBody != "" {
		renderedBody = RenderTemplate(renderedBody, v)
	}

	return s.emailSender.SendTestEmail(ctx, EmailTestSendInput{
		StoreName:    store.Name,
		StoreLogoURL: logoURL,
		ToEmail:      recipientEmail,
		Subject:      renderedSubject,
		BodyHTML:     renderedBody,
		StoreID:      storeID,
	})
}

// EmailSender abstracts the email client so notification.Service doesn't
// drag the lib/email package into its import graph during tests. The HTTP
// server wires the concrete *email.Client at boot via SetEmailSender.
type EmailSender interface {
	SendTestEmail(ctx context.Context, input EmailTestSendInput) error
}

// EmailTestSendInput is the slim payload the test endpoint hands to the email
// client. Body is empty when the merchant only customized the subject — the
// email layer falls back to a friendly placeholder body in that case.
type EmailTestSendInput struct {
	StoreName    string
	StoreLogoURL string
	ToEmail      string
	Subject      string
	BodyHTML     string

	// StoreID (optional) links the email audit trail row to the store.
	StoreID string
}

// SetEmailSender wires the concrete email client. Optional — when unset,
// SendTestEmail returns a clear error instead of crashing.
func (s *Service) SetEmailSender(sender EmailSender) {
	s.emailSender = sender
}

func defaultSubjectFor(notifType string, vars TemplateVariables) string {
	switch notifType {
	case "payment_confirmed":
		return fmt.Sprintf("Pedido #%s confirmado · %s", vars.NumeroPedido, vars.Loja)
	case "shipped":
		return fmt.Sprintf("Pedido #%s a caminho · %s", vars.NumeroPedido, vars.Loja)
	case "delivered":
		return fmt.Sprintf("Seu pedido #%s chegou · %s", vars.NumeroPedido, vars.Loja)
	default:
		return fmt.Sprintf("Teste · %s", vars.Loja)
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

// templateSection devolve o ponteiro para a seção de UM tipo dentro de
// Settings. Ponto único: mergeSettings, getTemplateSettings, o DTO do handler e
// os testes falam do mesmo mapeamento. Antes cada um repetia o seu switch, e
// foi assim que waitlist_notified existiu por meses no domínio sem existir no
// HTTP — inconfigurável, sem ninguém perceber.
func templateSection(s *Settings, t NotificationType) *TemplateSettings {
	if s == nil {
		return nil
	}
	switch t {
	case TypeCheckoutImmediate:
		return s.CheckoutImmediate
	case TypeItemAdded:
		return s.ItemAdded
	case TypeCheckoutReminder:
		return s.CheckoutReminder
	case TypeWaitlistNotified:
		return s.WaitlistNotified
	case TypeOutOfWindowScheduled:
		return s.OutOfWindowScheduled
	case TypeOutOfWindowSessionEnded:
		return s.OutOfWindowSessionEnded
	case TypeOutOfWindowEventEnded:
		return s.OutOfWindowEventEnded
	case TypeEventDeadlineStarted:
		return s.EventDeadlineStarted
	case TypeWaitlistUnfulfilled:
		return s.WaitlistUnfulfilled
	default:
		return nil
	}
}

// getTemplateSettings resolve o template efetivo de um tipo, caindo no default
// quando a loja nunca o customizou.
//
// O fallback vale para TODA chave, não só para waitlist_notified: o JSONB de
// toda loja existente antecede os cinco gatilhos novos, e sem o fallback eles
// nasceriam desligados em silêncio em 100% da base — um gatilho que "existe"
// mas nunca dispara é pior do que um que não existe.
func (s *Service) getTemplateSettings(settings *Settings, notifType NotificationType) *TemplateSettings {
	if section := templateSection(settings, notifType); section != nil {
		return section
	}
	defaults := DefaultSettings()
	return templateSection(&defaults, notifType)
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

// markUndelivered carimba "nem tentamos, e este é o motivo" (RN-38).
func (s *Service) markUndelivered(ctx context.Context, logID string, reason UndeliverableReason) {
	uid, ok := logUUID(logID)
	if !ok {
		return
	}
	if err := s.queries.MarkNotificationUndelivered(ctx, sqlc.MarkNotificationUndeliveredParams{
		ID:                uid,
		UndeliveredReason: pgtype.Text{String: string(reason), Valid: true},
	}); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mark notification undelivered",
			zap.String("log_id", logID), zap.Error(err))
	}
}

// stampUndeliveredReason acrescenta o motivo a uma linha que JÁ foi tentada e
// recusada — o status dela continua 'failed'.
func (s *Service) stampUndeliveredReason(ctx context.Context, logID string, reason UndeliverableReason) {
	uid, ok := logUUID(logID)
	if !ok {
		return
	}
	if err := s.queries.SetNotificationUndeliveredReason(ctx, sqlc.SetNotificationUndeliveredReasonParams{
		ID:                uid,
		UndeliveredReason: pgtype.Text{String: string(reason), Valid: true},
	}); err != nil {
		logger.From(ctx, s.logger).Warn("failed to stamp undelivered reason",
			zap.String("log_id", logID), zap.Error(err))
	}
}

// logUUID converte o id do log, tolerando o vazio: createLog pode ter falhado
// (best-effort), e nesse caso não há linha para atualizar.
func logUUID(logID string) (pgtype.UUID, bool) {
	if logID == "" {
		return pgtype.UUID{}, false
	}
	uid, err := parseUUID(logID)
	if err != nil {
		return pgtype.UUID{}, false
	}
	return uid, true
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, fmt.Errorf("invalid UUID: %s", s)
	}
	return uid, nil
}

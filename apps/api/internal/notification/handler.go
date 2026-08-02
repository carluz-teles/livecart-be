package notification

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Handler handles HTTP requests for notification settings.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new notification handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("notification-handler"),
	}
}

// RegisterRoutes registers notification routes.
func (h *Handler) RegisterRoutes(api fiber.Router) {
	notifications := api.Group("/notifications")
	notifications.Get("/settings", h.GetSettings)
	notifications.Put("/settings", h.UpdateSettings)
	notifications.Post("/preview", h.PreviewTemplate)
	notifications.Get("/variables", h.GetAvailableVariables)
	notifications.Get("/test/recipient", h.GetTestRecipient)
	notifications.Post("/test/setup", h.StartTestSetup)
	notifications.Post("/test", h.SendTest)
	notifications.Post("/test/email", h.SendTestEmail)
}

// GetSettingsResponse represents the response for getting notification settings.
// NOTE: Triggers (when to send) are now in cart_settings, not here.
type GetSettingsResponse struct {
	CheckoutImmediate *TemplateSettingsResponse      `json:"checkout_immediate"`
	ItemAdded         *TemplateSettingsResponse      `json:"item_added"`
	CheckoutReminder  *TemplateSettingsResponse      `json:"checkout_reminder"`
	PaymentConfirmed  *EmailTemplateSettingsResponse `json:"payment_confirmed,omitempty"`
	Shipped           *EmailTemplateSettingsResponse `json:"shipped,omitempty"`
	Delivered         *EmailTemplateSettingsResponse `json:"delivered,omitempty"`
	PaymentCancelled  *EmailTemplateSettingsResponse `json:"payment_cancelled,omitempty"`
	PaymentRefunded   *EmailTemplateSettingsResponse `json:"payment_refunded,omitempty"`
}

// TemplateSettingsResponse represents template settings in API responses.
// Only contains enabled + template. Triggers are in cart_settings.
type TemplateSettingsResponse struct {
	Enabled  bool   `json:"enabled"`
	Template string `json:"template"`
}

// EmailTemplateSettingsResponse mirrors EmailTemplateSettings on the wire.
// Subject and BodyHTML may be empty strings — empty means "use the BE
// default", non-empty means "merchant overrode this".
type EmailTemplateSettingsResponse struct {
	Enabled  bool   `json:"enabled"`
	Subject  string `json:"subject"`
	BodyHTML string `json:"body_html"`
}

// GetSettings returns notification settings for a store.
// @Summary Get notification settings
// @Description Returns notification settings for the authenticated user's store
// @Tags Notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} GetSettingsResponse
// @Failure 401 {object} httpx.Envelope
// @Failure 500 {object} httpx.Envelope
// @Router /stores/{storeId}/notifications/settings [get]
func (h *Handler) GetSettings(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	settings, err := h.service.GetSettings(c.Context(), storeID)
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to get notification settings",
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao buscar configurações de notificação")
	}

	return httpx.OK(c, toSettingsResponse(settings))
}

// UpdateSettingsRequest represents the request for updating notification settings.
// NOTE: Triggers (when to send) are now in cart_settings, not here.
type UpdateSettingsRequest struct {
	CheckoutImmediate *UpdateTemplateSettingsRequest      `json:"checkout_immediate"`
	ItemAdded         *UpdateTemplateSettingsRequest      `json:"item_added"`
	CheckoutReminder  *UpdateTemplateSettingsRequest      `json:"checkout_reminder"`
	PaymentConfirmed  *UpdateEmailTemplateSettingsRequest `json:"payment_confirmed,omitempty"`
	Shipped           *UpdateEmailTemplateSettingsRequest `json:"shipped,omitempty"`
	Delivered         *UpdateEmailTemplateSettingsRequest `json:"delivered,omitempty"`
	PaymentCancelled  *UpdateEmailTemplateSettingsRequest `json:"payment_cancelled,omitempty"`
	PaymentRefunded   *UpdateEmailTemplateSettingsRequest `json:"payment_refunded,omitempty"`
}

// UpdateTemplateSettingsRequest represents template settings in API requests.
// Only contains enabled + template. Triggers are in cart_settings.
type UpdateTemplateSettingsRequest struct {
	Enabled  bool   `json:"enabled"`
	Template string `json:"template" validate:"required,min=1,max=1500"`
}

// UpdateEmailTemplateSettingsRequest is the API shape for the post-payment
// email overrides. Empty strings are valid — they mean "fall back to default".
type UpdateEmailTemplateSettingsRequest struct {
	Enabled  bool   `json:"enabled"`
	Subject  string `json:"subject" validate:"max=200"`
	BodyHTML string `json:"body_html" validate:"max=20000"`
}

// UpdateSettings updates notification settings for a store.
// @Summary Update notification settings
// @Description Updates notification settings for the authenticated user's store
// @Tags Notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body UpdateSettingsRequest true "Notification settings"
// @Success 200 {object} GetSettingsResponse
// @Failure 400 {object} httpx.Envelope
// @Failure 401 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Failure 500 {object} httpx.Envelope
// @Router /stores/{storeId}/notifications/settings [put]
func (h *Handler) UpdateSettings(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req UpdateSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.ErrBadRequest("Corpo da requisição inválido")
	}

	// Convert request to domain settings
	settings := toSettingsFromRequest(&req)

	// Validate templates
	if settings.CheckoutImmediate != nil && settings.CheckoutImmediate.Enabled {
		if _, err := ValidateTemplate(settings.CheckoutImmediate.Template, SampleVariables()); err != nil {
			return httpx.ErrUnprocessable("Template checkout_immediate: " + err.Error())
		}
	}
	if settings.ItemAdded != nil && settings.ItemAdded.Enabled {
		if _, err := ValidateTemplate(settings.ItemAdded.Template, SampleVariables()); err != nil {
			return httpx.ErrUnprocessable("Template item_added: " + err.Error())
		}
	}
	if settings.CheckoutReminder != nil && settings.CheckoutReminder.Enabled {
		if _, err := ValidateTemplate(settings.CheckoutReminder.Template, SampleVariables()); err != nil {
			return httpx.ErrUnprocessable("Template checkout_reminder: " + err.Error())
		}
	}

	// UserContext (não Context) mantém o span OTEL e o logger.From — importa mais
	// agora que UpdateSettings faz uma leitura antes do merge.
	if err := h.service.UpdateSettings(c.UserContext(), storeID, settings); err != nil {
		logger.From(c.UserContext(), h.logger).Error("failed to update notification settings",
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao atualizar configurações de notificação")
	}

	return httpx.OK(c, toSettingsResponse(&settings))
}

// PreviewTemplateRequest represents the request for previewing a template.
type PreviewTemplateRequest struct {
	Template string `json:"template" validate:"required,min=1,max=1500"`
}

// PreviewTemplateResponse represents the response for previewing a template.
type PreviewTemplateResponse struct {
	Preview   string `json:"preview"`
	ByteCount int    `json:"byte_count"`
	MaxBytes  int    `json:"max_bytes"`
	IsValid   bool   `json:"is_valid"`
	Error     string `json:"error,omitempty"`
}

// PreviewTemplate renders a template with sample data.
// @Summary Preview a notification template
// @Description Renders a template with sample data and returns the preview
// @Tags Notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body PreviewTemplateRequest true "Template to preview"
// @Success 200 {object} PreviewTemplateResponse
// @Failure 400 {object} httpx.Envelope
// @Failure 401 {object} httpx.Envelope
// @Router /stores/{storeId}/notifications/preview [post]
func (h *Handler) PreviewTemplate(c *fiber.Ctx) error {
	var req PreviewTemplateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.ErrBadRequest("Corpo da requisição inválido")
	}

	if req.Template == "" {
		return httpx.ErrBadRequest("Template não pode estar vazio")
	}

	preview, byteCount, err := h.service.PreviewTemplate(req.Template)

	resp := PreviewTemplateResponse{
		Preview:   preview,
		ByteCount: byteCount,
		MaxBytes:  MaxMessageBytes,
		IsValid:   err == nil,
	}

	if err != nil {
		resp.Error = err.Error()
	}

	return httpx.OK(c, resp)
}

// GetAvailableVariablesResponse represents the response for available variables.
type GetAvailableVariablesResponse struct {
	Variables []VariableInfo `json:"variables"`
}

// VariableInfo describes a template variable.
type VariableInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
}

// GetAvailableVariables returns the list of available template variables.
// @Summary Get available template variables
// @Description Returns the list of variables that can be used in notification templates
// @Tags Notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} GetAvailableVariablesResponse
// @Router /stores/{storeId}/notifications/variables [get]
func (h *Handler) GetAvailableVariables(c *fiber.Ctx) error {
	// ?type=<template_type> escopa o catálogo às variáveis que fazem sentido
	// naquele template (o editor passa a rota atual). Sem type, devolve tudo.
	specs := VariablesForTemplate(c.Query("type"))

	variables := make([]VariableInfo, 0, len(specs))
	for _, s := range specs {
		variables = append(variables, VariableInfo{
			Name:        s.Name,
			Description: s.Description,
			Example:     s.Example,
		})
	}

	return httpx.OK(c, GetAvailableVariablesResponse{Variables: variables})
}

// Helper functions

func toSettingsResponse(s *Settings) GetSettingsResponse {
	resp := GetSettingsResponse{}

	if s.CheckoutImmediate != nil {
		resp.CheckoutImmediate = &TemplateSettingsResponse{
			Enabled:  s.CheckoutImmediate.Enabled,
			Template: s.CheckoutImmediate.Template,
		}
	}

	if s.ItemAdded != nil {
		resp.ItemAdded = &TemplateSettingsResponse{
			Enabled:  s.ItemAdded.Enabled,
			Template: s.ItemAdded.Template,
		}
	}

	if s.CheckoutReminder != nil {
		resp.CheckoutReminder = &TemplateSettingsResponse{
			Enabled:  s.CheckoutReminder.Enabled,
			Template: s.CheckoutReminder.Template,
		}
	}

	resp.PaymentConfirmed = toEmailResponse(s.PaymentConfirmed)
	resp.Shipped = toEmailResponse(s.Shipped)
	resp.Delivered = toEmailResponse(s.Delivered)
	resp.PaymentCancelled = toEmailResponse(s.PaymentCancelled)
	resp.PaymentRefunded = toEmailResponse(s.PaymentRefunded)

	return resp
}

func toEmailResponse(e *EmailTemplateSettings) *EmailTemplateSettingsResponse {
	if e == nil {
		return nil
	}
	return &EmailTemplateSettingsResponse{
		Enabled:  e.Enabled,
		Subject:  e.Subject,
		BodyHTML: e.BodyHTML,
	}
}

func toSettingsFromRequest(req *UpdateSettingsRequest) Settings {
	settings := Settings{}

	if req.CheckoutImmediate != nil {
		settings.CheckoutImmediate = &TemplateSettings{
			Enabled:  req.CheckoutImmediate.Enabled,
			Template: req.CheckoutImmediate.Template,
		}
	}

	if req.ItemAdded != nil {
		settings.ItemAdded = &TemplateSettings{
			Enabled:  req.ItemAdded.Enabled,
			Template: req.ItemAdded.Template,
		}
	}

	if req.CheckoutReminder != nil {
		settings.CheckoutReminder = &TemplateSettings{
			Enabled:  req.CheckoutReminder.Enabled,
			Template: req.CheckoutReminder.Template,
		}
	}

	settings.PaymentConfirmed = fromEmailRequest(req.PaymentConfirmed)
	settings.Shipped = fromEmailRequest(req.Shipped)
	settings.Delivered = fromEmailRequest(req.Delivered)
	settings.PaymentCancelled = fromEmailRequest(req.PaymentCancelled)
	settings.PaymentRefunded = fromEmailRequest(req.PaymentRefunded)

	return settings
}

func fromEmailRequest(r *UpdateEmailTemplateSettingsRequest) *EmailTemplateSettings {
	if r == nil {
		return nil
	}
	return &EmailTemplateSettings{
		Enabled:  r.Enabled,
		Subject:  r.Subject,
		BodyHTML: r.BodyHTML,
	}
}

// =============================================================================
// Test recipient handlers
// =============================================================================

// TestRecipientResponse describes the current setup state for a store.
type TestRecipientResponse struct {
	Configured      bool       `json:"configured"`
	Handle          string     `json:"handle,omitempty"`
	SetupCode       string     `json:"setup_code,omitempty"`
	SetupExpiresAt  *time.Time `json:"setup_expires_at,omitempty"`
	SetupCodeActive bool       `json:"setup_code_active"`
}

// SendTestRequest is the body for POST /notifications/test.
type SendTestRequest struct {
	Type     string `json:"type"`
	Template string `json:"template"`
}

func toTestRecipientResponse(r *TestRecipient) TestRecipientResponse {
	resp := TestRecipientResponse{
		Configured:      r.Configured(),
		Handle:          r.Handle,
		SetupCode:       r.SetupCode,
		SetupCodeActive: r.SetupActive(time.Now()),
	}
	if !r.SetupExpires.IsZero() {
		expires := r.SetupExpires
		resp.SetupExpiresAt = &expires
	}
	return resp
}

// GetTestRecipient returns the current test recipient state for a store.
func (h *Handler) GetTestRecipient(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	recipient, err := h.service.GetTestRecipient(c.Context(), storeID)
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to load test recipient",
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao carregar destinatário de teste")
	}
	return httpx.OK(c, toTestRecipientResponse(recipient))
}

// StartTestSetup generates a fresh setup code and returns it so the lojista
// can DM it from their personal IG to the store's business account.
func (h *Handler) StartTestSetup(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	recipient, err := h.service.StartTestRecipientSetup(c.Context(), storeID)
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to start test recipient setup",
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao iniciar configuração do destinatário")
	}
	return httpx.OK(c, toTestRecipientResponse(recipient))
}

// SendTestEmailRequest is the payload for POST /notifications/test/email.
// recipient_email is the address to ship the test to (the FE auto-fills
// this with the lojista's own Clerk email so it's a one-click flow). The
// subject + body_html mirror the merchant editor's two fields.
type SendTestEmailRequest struct {
	Type           string `json:"type"` // payment_confirmed | shipped | delivered
	Subject        string `json:"subject"`
	BodyHTML       string `json:"body_html"`
	RecipientEmail string `json:"recipient_email"`
}

// SendTestEmail renders the merchant's draft template with sample variables
// and dispatches it via Resend to the supplied recipient. Used by the
// "Testar envio" button in the post-payment editor.
func (h *Handler) SendTestEmail(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req SendTestEmailRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.ErrBadRequest("Corpo da requisição inválido")
	}
	if req.RecipientEmail == "" {
		return httpx.ErrBadRequest("Email destinatário é obrigatório")
	}
	if req.Subject == "" && req.BodyHTML == "" {
		return httpx.ErrBadRequest("Assunto ou corpo precisam estar preenchidos")
	}

	if err := h.service.SendTestEmail(c.Context(), storeID, req.Type, req.Subject, req.BodyHTML, req.RecipientEmail); err != nil {
		logger.From(c.Context(), h.logger).Warn("failed to send test email",
			zap.String("to", req.RecipientEmail),
			zap.String("type", req.Type),
			zap.Error(err),
		)
		return httpx.ErrInternal("Não foi possível enviar o email de teste")
	}
	return httpx.OK(c, fiber.Map{"sent": true})
}

// SendTest dispatches a real Instagram DM rendered with sample data to the
// configured test recipient.
func (h *Handler) SendTest(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req SendTestRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.ErrBadRequest("Corpo da requisição inválido")
	}
	if req.Type == "" || req.Template == "" {
		return httpx.ErrBadRequest("Type e template são obrigatórios")
	}

	if err := h.service.SendTest(c.Context(), storeID, NotificationType(req.Type), req.Template); err != nil {
		if errors.Is(err, ErrTestRecipientNotConfigured) {
			return httpx.ErrUnprocessable("Configure o destinatário de teste antes de enviar.")
		}
		logger.From(c.Context(), h.logger).Warn("failed to send test notification",
			zap.String("type", req.Type),
			zap.Error(err),
		)
		return httpx.ErrInternal("Não foi possível enviar a notificação de teste")
	}
	return httpx.OK(c, fiber.Map{"sent": true})
}

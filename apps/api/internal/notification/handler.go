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
	notifications.Get("/undelivered", h.ListUndelivered)
	notifications.Get("/test/recipient", h.GetTestRecipient)
	notifications.Post("/test/setup", h.StartTestSetup)
	notifications.Post("/test", h.SendTest)
	notifications.Post("/test/email", h.SendTestEmail)
}

// GetSettingsResponse represents the response for getting notification settings.
// NOTE: Triggers (when to send) are now in cart_settings, not here.
type GetSettingsResponse struct {
	CheckoutImmediate *TemplateSettingsResponse `json:"checkout_immediate"`
	ItemAdded         *TemplateSettingsResponse `json:"item_added"`
	CheckoutReminder  *TemplateSettingsResponse `json:"checkout_reminder"`

	// Os cinco gatilhos da RN-28 mais waitlist_notified. Este DTO é o motivo
	// pelo qual waitlist_notified existiu por meses sem ser configurável: o
	// domínio tinha a chave, o HTTP não, e nenhuma UI conseguia nem ler nem
	// escrever. Chave nova sem entrada aqui é chave morta.
	OutOfWindowScheduled    *TemplateSettingsResponse `json:"out_of_window_scheduled,omitempty"`
	OutOfWindowSessionEnded *TemplateSettingsResponse `json:"out_of_window_session_ended,omitempty"`
	OutOfWindowEventEnded   *TemplateSettingsResponse `json:"out_of_window_event_ended,omitempty"`
	EventDeadlineStarted    *TemplateSettingsResponse `json:"event_deadline_started,omitempty"`
	WaitlistUnfulfilled     *TemplateSettingsResponse `json:"waitlist_unfulfilled,omitempty"`
	WaitlistJoined          *TemplateSettingsResponse `json:"waitlist_joined,omitempty"`

	PaymentConfirmed *EmailTemplateSettingsResponse `json:"payment_confirmed,omitempty"`
	Shipped          *EmailTemplateSettingsResponse `json:"shipped,omitempty"`
	Delivered        *EmailTemplateSettingsResponse `json:"delivered,omitempty"`
	PaymentCancelled *EmailTemplateSettingsResponse `json:"payment_cancelled,omitempty"`
	PaymentRefunded  *EmailTemplateSettingsResponse `json:"payment_refunded,omitempty"`
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
	CheckoutImmediate *UpdateTemplateSettingsRequest `json:"checkout_immediate"`
	ItemAdded         *UpdateTemplateSettingsRequest `json:"item_added"`
	CheckoutReminder  *UpdateTemplateSettingsRequest `json:"checkout_reminder"`

	OutOfWindowScheduled    *UpdateTemplateSettingsRequest `json:"out_of_window_scheduled,omitempty"`
	OutOfWindowSessionEnded *UpdateTemplateSettingsRequest `json:"out_of_window_session_ended,omitempty"`
	OutOfWindowEventEnded   *UpdateTemplateSettingsRequest `json:"out_of_window_event_ended,omitempty"`
	EventDeadlineStarted    *UpdateTemplateSettingsRequest `json:"event_deadline_started,omitempty"`
	WaitlistUnfulfilled     *UpdateTemplateSettingsRequest `json:"waitlist_unfulfilled,omitempty"`
	WaitlistJoined          *UpdateTemplateSettingsRequest `json:"waitlist_joined,omitempty"`

	PaymentConfirmed *UpdateEmailTemplateSettingsRequest `json:"payment_confirmed,omitempty"`
	Shipped          *UpdateEmailTemplateSettingsRequest `json:"shipped,omitempty"`
	Delivered        *UpdateEmailTemplateSettingsRequest `json:"delivered,omitempty"`
	PaymentCancelled *UpdateEmailTemplateSettingsRequest `json:"payment_cancelled,omitempty"`
	PaymentRefunded  *UpdateEmailTemplateSettingsRequest `json:"payment_refunded,omitempty"`
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

	// Valida todo template enviado E habilitado, pela mesma tabela que fez a
	// conversão. Antes eram três `if` copiados; uma chave nova entrava sem
	// validação de tamanho e só estourava na hora do envio, quando o
	// comprador é quem paga pela mensagem truncada.
	for _, sec := range dmSections {
		section := templateSection(&settings, sec.Type)
		if section == nil || !section.Enabled {
			continue
		}
		if _, err := ValidateTemplate(section.Template, SampleVariables()); err != nil {
			return httpx.ErrUnprocessable("Template " + string(sec.Type) + ": " + err.Error())
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

	// Relê depois de gravar em vez de devolver o que veio no corpo. O corpo é
	// PARCIAL por natureza (é o que o merge existe para tratar), então
	// devolvê-lo faria a resposta afirmar que as chaves não enviadas estão no
	// default — quando o que está gravado é a customização do lojista. O FE que
	// usasse a resposta para repopular o formulário apagaria a configuração
	// dele no save seguinte.
	saved, err := h.service.GetSettings(c.UserContext(), storeID)
	if err != nil {
		logger.From(c.UserContext(), h.logger).Error("failed to reload notification settings after update",
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao atualizar configurações de notificação")
	}

	return httpx.OK(c, toSettingsResponse(saved))
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

// ListUndeliveredResponse é o payload do painel "compradores não avisados".
// Traz o total junto com a lista porque as duas perguntas do copy deck ("{n}
// compradores não puderam ser avisados" e a lista em si) são a mesma consulta —
// dois endpoints seriam duas fontes que podem discordar entre um render e outro.
type ListUndeliveredResponse struct {
	Total   int                `json:"total"`
	Entries []UndeliveredEntry `json:"entries"`
}

// ListUndelivered returns the buyers that could not be notified for an event.
// @Summary      List undelivered notifications for an event
// @Description  RN-38: buyers whose Instagram reply window had already closed
// @Tags         Notifications
// @Produce      json
// @Security     BearerAuth
// @Param        storeId path string true "Store UUID"
// @Param        eventId query string true "Event UUID"
// @Success      200 {object} httpx.Envelope{data=ListUndeliveredResponse}
// @Failure      400 {object} httpx.Envelope
// @Router       /stores/{storeId}/notifications/undelivered [get]
func (h *Handler) ListUndelivered(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	eventID := c.Query("eventId")
	if eventID == "" {
		return httpx.ErrBadRequest("eventId é obrigatório")
	}

	entries, err := h.service.ListUndelivered(c.UserContext(), storeID, eventID)
	if err != nil {
		logger.From(c.UserContext(), h.logger).Error("failed to list undelivered notifications",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao listar mensagens não entregues")
	}

	// O total sai do tamanho da lista, e não de uma segunda consulta: a lista
	// já é por comprador (DISTINCT ON), então contar linhas aqui e contar
	// pessoas no banco são o mesmo número por construção — e um número só não
	// pode divergir de si mesmo.
	return httpx.OK(c, ListUndeliveredResponse{
		Total:   len(entries),
		Entries: entries,
	})
}

// Helper functions

// dmSection amarra, num lugar só, as três representações de uma chave de
// template de DM: o campo do Request, o campo do Settings e o campo da
// Response. É a tabela que impede o buraco do waitlist_notified de se repetir —
// acrescentar uma linha aqui liga a chave nova nos três lados de uma vez, e
// esquecer de acrescentá-la deixa de ser um erro silencioso porque o
// arquivo não tem mais nenhum outro lugar onde a chave apareceria.
type dmSection struct {
	Type    NotificationType
	fromReq func(*UpdateSettingsRequest) *UpdateTemplateSettingsRequest
	set     func(*Settings, *TemplateSettings)
	setResp func(*GetSettingsResponse, *TemplateSettingsResponse)
}

var dmSections = []dmSection{
	{TypeCheckoutImmediate,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.CheckoutImmediate },
		func(s *Settings, t *TemplateSettings) { s.CheckoutImmediate = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.CheckoutImmediate = t }},
	{TypeItemAdded,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.ItemAdded },
		func(s *Settings, t *TemplateSettings) { s.ItemAdded = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.ItemAdded = t }},
	{TypeCheckoutReminder,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.CheckoutReminder },
		func(s *Settings, t *TemplateSettings) { s.CheckoutReminder = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.CheckoutReminder = t }},
	{TypeOutOfWindowScheduled,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.OutOfWindowScheduled },
		func(s *Settings, t *TemplateSettings) { s.OutOfWindowScheduled = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.OutOfWindowScheduled = t }},
	{TypeOutOfWindowSessionEnded,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.OutOfWindowSessionEnded },
		func(s *Settings, t *TemplateSettings) { s.OutOfWindowSessionEnded = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.OutOfWindowSessionEnded = t }},
	{TypeOutOfWindowEventEnded,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.OutOfWindowEventEnded },
		func(s *Settings, t *TemplateSettings) { s.OutOfWindowEventEnded = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.OutOfWindowEventEnded = t }},
	{TypeEventDeadlineStarted,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.EventDeadlineStarted },
		func(s *Settings, t *TemplateSettings) { s.EventDeadlineStarted = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.EventDeadlineStarted = t }},
	{TypeWaitlistUnfulfilled,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.WaitlistUnfulfilled },
		func(s *Settings, t *TemplateSettings) { s.WaitlistUnfulfilled = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.WaitlistUnfulfilled = t }},
	{TypeWaitlistJoined,
		func(r *UpdateSettingsRequest) *UpdateTemplateSettingsRequest { return r.WaitlistJoined },
		func(s *Settings, t *TemplateSettings) { s.WaitlistJoined = t },
		func(r *GetSettingsResponse, t *TemplateSettingsResponse) { r.WaitlistJoined = t }},
}

func toSettingsResponse(s *Settings) GetSettingsResponse {
	resp := GetSettingsResponse{}

	for _, sec := range dmSections {
		section := templateSection(s, sec.Type)
		if section == nil {
			// A loja nunca customizou esta chave. Devolver o DEFAULT, e não
			// nil, é o que faz o card aparecer na aba de Comunicações já
			// preenchido com o texto que o comprador de fato recebe — nil
			// deixaria o lojista editando um campo vazio que não corresponde
			// ao envio real.
			defaults := DefaultSettings()
			section = templateSection(&defaults, sec.Type)
		}
		if section == nil {
			continue
		}
		sec.setResp(&resp, &TemplateSettingsResponse{
			Enabled:  section.Enabled,
			Template: section.Template,
		})
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

	for _, sec := range dmSections {
		in := sec.fromReq(req)
		if in == nil {
			// nil = "não enviado". Fica nil no Settings para que mergeSettings
			// preserve o que está gravado.
			continue
		}
		sec.set(&settings, &TemplateSettings{
			Enabled:  in.Enabled,
			Template: in.Template,
		})
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

package notification

import (
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
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
}

// GetSettingsResponse represents the response for getting notification settings.
type GetSettingsResponse struct {
	CheckoutImmediate *TemplateSettingsResponse `json:"checkout_immediate"`
	ItemAdded         *TemplateSettingsResponse `json:"item_added"`
	CheckoutReminder  *TemplateSettingsResponse `json:"checkout_reminder"`
}

// TemplateSettingsResponse represents template settings in API responses.
type TemplateSettingsResponse struct {
	Enabled         bool   `json:"enabled"`
	OnFirstItem     bool   `json:"on_first_item,omitempty"`
	OnNewItems      bool   `json:"on_new_items,omitempty"`
	CooldownSeconds int    `json:"cooldown_seconds,omitempty"`
	Template        string `json:"template"`
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
		h.logger.Error("failed to get notification settings",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao buscar configurações de notificação")
	}

	return httpx.OK(c, toSettingsResponse(settings))
}

// UpdateSettingsRequest represents the request for updating notification settings.
type UpdateSettingsRequest struct {
	CheckoutImmediate *UpdateTemplateSettingsRequest `json:"checkout_immediate"`
	ItemAdded         *UpdateTemplateSettingsRequest `json:"item_added"`
	CheckoutReminder  *UpdateTemplateSettingsRequest `json:"checkout_reminder"`
}

// UpdateTemplateSettingsRequest represents template settings in API requests.
type UpdateTemplateSettingsRequest struct {
	Enabled         bool   `json:"enabled"`
	OnFirstItem     bool   `json:"on_first_item,omitempty"`
	OnNewItems      bool   `json:"on_new_items,omitempty"`
	CooldownSeconds int    `json:"cooldown_seconds,omitempty"`
	Template        string `json:"template" validate:"required,min=1,max=1500"`
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

	if err := h.service.UpdateSettings(c.Context(), storeID, settings); err != nil {
		h.logger.Error("failed to update notification settings",
			zap.String("store_id", storeID),
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
	sample := SampleVariables()

	variables := []VariableInfo{
		{Name: "{handle}", Description: "Nome de usuário do comprador", Example: sample.Handle},
		{Name: "{produto}", Description: "Nome do produto adicionado", Example: sample.Produto},
		{Name: "{keyword}", Description: "Palavra-chave do produto", Example: sample.Keyword},
		{Name: "{quantidade}", Description: "Quantidade do último item", Example: "2"},
		{Name: "{total_itens}", Description: "Total de itens no carrinho", Example: "3"},
		{Name: "{total}", Description: "Valor total formatado", Example: sample.Total},
		{Name: "{link}", Description: "Link de checkout", Example: sample.Link},
		{Name: "{loja}", Description: "Nome da loja", Example: sample.Loja},
		{Name: "{expira_em}", Description: "Tempo até expiração", Example: sample.ExpiraEm},
		{Name: "{live_titulo}", Description: "Título da live", Example: sample.LiveTitulo},
	}

	return httpx.OK(c, GetAvailableVariablesResponse{Variables: variables})
}

// Helper functions

func toSettingsResponse(s *Settings) GetSettingsResponse {
	resp := GetSettingsResponse{}

	if s.CheckoutImmediate != nil {
		resp.CheckoutImmediate = &TemplateSettingsResponse{
			Enabled:         s.CheckoutImmediate.Enabled,
			OnFirstItem:     s.CheckoutImmediate.OnFirstItem,
			OnNewItems:      s.CheckoutImmediate.OnNewItems,
			CooldownSeconds: s.CheckoutImmediate.CooldownSeconds,
			Template:        s.CheckoutImmediate.Template,
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

	return resp
}

func toSettingsFromRequest(req *UpdateSettingsRequest) Settings {
	settings := Settings{}

	if req.CheckoutImmediate != nil {
		settings.CheckoutImmediate = &TemplateSettings{
			Enabled:         req.CheckoutImmediate.Enabled,
			OnFirstItem:     req.CheckoutImmediate.OnFirstItem,
			OnNewItems:      req.CheckoutImmediate.OnNewItems,
			CooldownSeconds: req.CheckoutImmediate.CooldownSeconds,
			Template:        req.CheckoutImmediate.Template,
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

	return settings
}

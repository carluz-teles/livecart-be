package notification

import "time"

// NotificationType represents the type of notification being sent.
type NotificationType string

const (
	// TypeCheckoutImmediate is sent when a cart is first created.
	TypeCheckoutImmediate NotificationType = "checkout_immediate"
	// TypeItemAdded is sent when a new item is added to an existing cart.
	TypeItemAdded NotificationType = "item_added"
	// TypeCheckoutReminder is sent when the live ends (current behavior).
	TypeCheckoutReminder NotificationType = "checkout_reminder"
)

// NotificationChannel represents the channel used to send notifications.
type NotificationChannel string

const (
	ChannelInstagramDM NotificationChannel = "instagram_dm"
	ChannelWhatsApp    NotificationChannel = "whatsapp"
	ChannelEmail       NotificationChannel = "email"
)

// NotificationStatus represents the status of a notification attempt.
type NotificationStatus string

const (
	StatusPending  NotificationStatus = "pending"
	StatusSent     NotificationStatus = "sent"
	StatusFailed   NotificationStatus = "failed"
	StatusSkipped  NotificationStatus = "skipped"
	StatusCooldown NotificationStatus = "cooldown"
)

// Settings represents the notification settings for a store.
// NOTE: Triggers (when to send) are now in CartMessageSettings.
// This struct only contains templates (what to send).
type Settings struct {
	CheckoutImmediate *TemplateSettings `json:"checkout_immediate"`
	ItemAdded         *TemplateSettings `json:"item_added"`
	CheckoutReminder  *TemplateSettings `json:"checkout_reminder"`
}

// TemplateSettings represents settings for a specific notification type.
// Contains only template configuration - triggers are in CartMessageSettings.
type TemplateSettings struct {
	Enabled  bool   `json:"enabled"`
	Template string `json:"template"`
}

// CartMessageSettings represents when/how to send automatic messages.
// These are stored in stores.cart_* columns and control notification triggers.
type CartMessageSettings struct {
	RealTimeCart              bool `json:"realTimeCart"`
	MessageCooldownSeconds    int  `json:"messageCooldownSeconds"`
	SendExpirationReminder    bool `json:"sendExpirationReminder"`
	ExpirationReminderMinutes int  `json:"expirationReminderMinutes"`
}

// DefaultSettings returns the default notification settings.
// NOTE: These are just templates. Triggers (when to send) are in cart_settings.
func DefaultSettings() Settings {
	return Settings{
		CheckoutImmediate: &TemplateSettings{
			Enabled:  true,
			Template: "Olá {handle}! 🛒\n\nVocê pediu {produto} na live!\n\nTotal: {total}\n\nFinalize aqui: {link}\n\n⏰ Válido por {expira_em}",
		},
		ItemAdded: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! ➕\n\nNovo item adicionado: {produto}\n\nSeu carrinho agora tem {total_itens} itens\nTotal: {total}\n\nFinalize: {link}",
		},
		CheckoutReminder: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! 🛒\n\nSeu carrinho com {total_itens} itens está esperando!\n\nTotal: {total}\n\nFinalize aqui: {link}\n\n⏰ Válido por {expira_em}",
		},
	}
}

// DefaultCartMessageSettings returns the default cart message settings.
func DefaultCartMessageSettings() CartMessageSettings {
	return CartMessageSettings{
		RealTimeCart:              true,
		MessageCooldownSeconds:    30,
		SendExpirationReminder:    true,
		ExpirationReminderMinutes: 15,
	}
}

// TemplateVariables contains all available variables for template rendering.
type TemplateVariables struct {
	Handle       string // @username
	Produto      string // Product name
	Keyword      string // Product keyword
	Quantidade   int    // Quantity of last item
	TotalItens   int    // Total items in cart
	Total        string // Formatted total (e.g., "R$ 199,90")
	TotalCents   int64  // Total in cents
	Link         string // Checkout URL
	Loja         string // Store name
	ExpiraEm     string // Expiry time (e.g., "48 horas")
	LiveTitulo   string // Event title
}

// LogEntry represents a notification log entry.
type LogEntry struct {
	ID               string
	StoreID          string
	EventID          *string
	CartID           *string
	PlatformUserID   string
	PlatformHandle   string
	NotificationType NotificationType
	Channel          NotificationChannel
	Status           NotificationStatus
	MessageText      string
	ErrorMessage     *string
	CreatedAt        time.Time
	SentAt           *time.Time
}

// SendInput represents input for sending a notification.
type SendInput struct {
	StoreID          string
	EventID          string
	CartID           string
	CartToken        string
	PlatformUserID   string
	PlatformHandle   string
	NotificationType NotificationType
	Variables        TemplateVariables
}

// SendResult represents the result of a notification send attempt.
type SendResult struct {
	LogID       string
	Status      NotificationStatus
	MessageText string
	Error       error
}

// Instagram DM limit is 1000 bytes
const MaxMessageBytes = 1000

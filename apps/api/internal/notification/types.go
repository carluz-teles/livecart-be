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
	// TypeWaitlistNotified is fired when a waitlisted item is promoted: the
	// next-in-line customer was just bumped to "notified" and has the
	// configurable TTL window to finalize before the slot is released.
	TypeWaitlistNotified NotificationType = "waitlist_notified"
	// TypeCartRecovery is the post-expiration WhatsApp recovery message
	// (PRD 006): the cart expired unpaid, a fresh checkout was regenerated
	// and the approved template goes out with the new link.
	TypeCartRecovery NotificationType = "cart_recovery"
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
	StatusPending NotificationStatus = "pending"
	StatusSent    NotificationStatus = "sent"
	StatusFailed  NotificationStatus = "failed"
	StatusSkipped NotificationStatus = "skipped"
)

// Settings represents the notification settings for a store.
// NOTE: Triggers (when to send) are now in CartMessageSettings.
// This struct only contains templates (what to send).
//
// Cart-flow notifications (CheckoutImmediate, ItemAdded, CheckoutReminder)
// go through Instagram DM. Post-payment notifications (PaymentConfirmed,
// Shipped, Delivered) go through Email — different channel, different
// shape (subject + body_html instead of plain template). The two surfaces
// coexist on the same JSONB column for now.
type Settings struct {
	CheckoutImmediate *TemplateSettings      `json:"checkout_immediate"`
	ItemAdded         *TemplateSettings      `json:"item_added"`
	CheckoutReminder  *TemplateSettings      `json:"checkout_reminder"`
	WaitlistNotified  *TemplateSettings      `json:"waitlist_notified,omitempty"`
	PaymentConfirmed  *EmailTemplateSettings `json:"payment_confirmed,omitempty"`
	Shipped           *EmailTemplateSettings `json:"shipped,omitempty"`
	Delivered         *EmailTemplateSettings `json:"delivered,omitempty"`
	PaymentCancelled  *EmailTemplateSettings `json:"payment_cancelled,omitempty"`
	PaymentRefunded   *EmailTemplateSettings `json:"payment_refunded,omitempty"`
	// CartRecovery (PRD 006): WhatsApp post-expiration recovery. Must stay in
	// this struct so UpdateSettings round-trips the JSONB without dropping it.
	CartRecovery *CartRecoverySettings `json:"cart_recovery,omitempty"`
}

// CartRecoverySettings configures the WhatsApp recovery worker per store.
// The template text here is informational for the merchant UI — the message
// actually sent is the Meta-approved content template on the Twilio side.
type CartRecoverySettings struct {
	Enabled            bool   `json:"enabled"`
	DelayMinutes       int    `json:"delay_minutes"`
	MaxAttempts        int    `json:"max_attempts"`
	QuietHoursStart    int    `json:"quiet_hours_start"`
	QuietHoursEnd      int    `json:"quiet_hours_end"`
	RecoverEndedEvents bool   `json:"recover_ended_events"`
	Template           string `json:"template"`
}

// TemplateSettings represents settings for a specific notification type.
// Contains only template configuration - triggers are in CartMessageSettings.
type TemplateSettings struct {
	Enabled  bool   `json:"enabled"`
	Template string `json:"template"`
}

// EmailTemplateSettings is the shape of post-payment email overrides.
// Subject and BodyHTML are merchant-customizable strings with the same
// {variable} substitution as the IG DM templates. When fields are empty,
// the postcheckout layer uses its hardcoded defaults.
type EmailTemplateSettings struct {
	Enabled  bool   `json:"enabled"`
	Subject  string `json:"subject"`
	BodyHTML string `json:"body_html"`
}

// CartMessageSettings represents when/how to send automatic messages.
// These are stored in stores.cart_* columns and control notification triggers.
type CartMessageSettings struct {
	RealTimeCart              bool `json:"realTimeCart"`
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
		WaitlistNotified: &TemplateSettings{
			Enabled:  true,
			Template: "Boa notícia, {handle}! ✅\n\nO produto {produto} que você esperava acabou de liberar.\n\nFinalize a compra: {link}\n\n⏰ Você tem {expira_em} antes do item voltar para a fila.",
		},
		PaymentConfirmed: &EmailTemplateSettings{
			Enabled: true,
		},
		Shipped: &EmailTemplateSettings{
			Enabled: true,
		},
		Delivered: &EmailTemplateSettings{
			Enabled: true,
		},
		PaymentCancelled: &EmailTemplateSettings{
			Enabled: true,
		},
		PaymentRefunded: &EmailTemplateSettings{
			Enabled: true,
		},
	}
}

// DefaultCartMessageSettings returns the default cart message settings.
func DefaultCartMessageSettings() CartMessageSettings {
	return CartMessageSettings{
		RealTimeCart:              true,
		SendExpirationReminder:    true,
		ExpirationReminderMinutes: 15,
	}
}

// TemplateVariables contains all available variables for template rendering.
type TemplateVariables struct {
	Handle     string // @username
	Produto    string // Product name
	Keyword    string // Product keyword
	Quantidade int    // Quantity of last item
	TotalItens int    // Total items in cart
	Total      string // Formatted total (e.g., "R$ 199,90")
	TotalCents int64  // Total in cents
	Link       string // Checkout URL
	Loja       string // Store name
	ExpiraEm   string // Expiry time (e.g., "48 horas")
	LiveTitulo string // Event title

	// FormaPagamento: "PIX" / "Cartão" — populada no pós-pagamento.
	FormaPagamento string
	// NomeCliente: nome preenchido no checkout (fallback: handle).
	NomeCliente string
	// ListaProdutos: tabela HTML dos itens (e-mails); vazia no fluxo de DM.
	ListaProdutos string
	// EnderecoEntrega: linha única "Rua X, 123 — Cidade/UF".
	EnderecoEntrega string
	// PrazoEntrega: "até N dias úteis" (da cotação de frete).
	PrazoEntrega string
	// ValorFrete: "R$ 18,90".
	ValorFrete string

	// Post-payment variables. Empty for cart-flow notifications, populated
	// by the postcheckout package when sending receipt/shipped/delivered.
	NumeroPedido  string // Order short_id, formatted as "1234"
	TrackingCode  string // Carrier tracking code, when known
	Transportadora string // Carrier name + service ("Sedex via Correios")
	LinkPedido    string // Public order page URL with tracking_token
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
	StoreID           string
	EventID           string
	CartID            string
	CartToken         string
	PlatformUserID    string
	PlatformHandle    string
	PlatformCommentID string // Instagram comment ID for reply (if available)
	NotificationType  NotificationType
	Variables         TemplateVariables
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

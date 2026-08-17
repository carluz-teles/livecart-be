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
	// TypeCartRecovery is the post-expiration WhatsApp recovery message
	// (PRD 006): the cart expired unpaid, a fresh checkout was regenerated
	// and the approved template goes out with the new link.
	TypeCartRecovery NotificationType = "cart_recovery"

	// Os cinco gatilhos da RN-28. Os três primeiros são o MESMO momento
	// ("comentou fora da janela") visto de três lugares diferentes, e por isso
	// são três chaves e não uma: a resposta certa muda em cada um. Só o
	// out_of_window_session_ended fala com alguém que AINDA pode comprar
	// (a campanha continua, só esta transmissão acabou), então o texto padrão
	// dele redireciona em vez de negar.

	// TypeOutOfWindowScheduled responde quem comenta antes de starts_at.
	TypeOutOfWindowScheduled NotificationType = "out_of_window_scheduled"
	// TypeOutOfWindowSessionEnded responde quem comenta numa transmissão
	// encerrada enquanto a campanha segue aberta.
	TypeOutOfWindowSessionEnded NotificationType = "out_of_window_session_ended"
	// TypeOutOfWindowEventEnded responde quem comenta depois de a campanha
	// fechar.
	TypeOutOfWindowEventEnded NotificationType = "out_of_window_event_ended"
	// TypeEventDeadlineStarted avisa que a campanha fechou e o prazo para
	// pagar começou a correr. É o único momento em que o comprador PRECISA
	// agir, e por isso o de maior conversão do deck.
	TypeEventDeadlineStarted NotificationType = "event_deadline_started"
	// TypeWaitlistUnfulfilled avisa que o item que o comprador esperava na
	// fila não liberou até o fim da campanha. Notícia ruim: o texto padrão
	// sempre aponta para o que ainda existe (o resto do carrinho) ou para o
	// futuro, nunca termina no "não deu".
	TypeWaitlistUnfulfilled NotificationType = "waitlist_unfulfilled"

	// TypeWaitlistJoined é o momento em que o comprador ENTRA na fila: pediu um
	// item que o estoque não cobriu, no todo ou em parte.
	//
	// Sem esta chave o comprador recebia o texto de item_added — "Adicionei X
	// ao seu carrinho" — para um item que NÃO foi adicionado, e ficava sem
	// saber que estava numa fila. Os dois gatilhos de fila que já existiam
	// cobrem só o desfecho (waitlist_notified quando libera,
	// waitlist_unfulfilled quando não libera); a entrada não tinha voz.
	TypeWaitlistJoined NotificationType = "waitlist_joined"
)

// CartFlowTypes são os tipos entregues por Instagram (DM ou private reply) no
// fluxo de carrinho. Existe para os pontos que precisam iterar sobre "tudo que
// o lojista configura na aba de Comunicações" sem repetir a lista — o custo de
// esquecer uma chave nova em um desses pontos é ela virar inconfigurável em
// silêncio, que é exatamente o que aconteceu com waitlist_notified.
var CartFlowTypes = []NotificationType{
	TypeCheckoutImmediate,
	TypeItemAdded,
	TypeOutOfWindowScheduled,
	TypeOutOfWindowSessionEnded,
	TypeOutOfWindowEventEnded,
	TypeEventDeadlineStarted,
	TypeWaitlistJoined,
	TypeWaitlistUnfulfilled,
	TypeCheckoutReminder,
}

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
	// StatusUndelivered (RN-38) é o estado de "sabíamos de antemão que não ia
	// entregar, então nem tentamos". Distinto de 'failed' de propósito: failed
	// é o Instagram recusando uma tentativa real (rede, token, formato), e
	// pode ser retentado; undelivered é uma porta fechada por regra da
	// plataforma — o lojista precisa chamar essa pessoa na mão. Misturar os
	// dois esconde a lista que a RN-38 existe para mostrar.
	StatusUndelivered NotificationStatus = "undelivered"
)

// UndeliverableReason é o motivo, legível pelo lojista, de uma mensagem não
// entregue. Fica em coluna própria (undelivered_reason) e não em error_message
// porque error_message carrega o erro cru do Graph — texto que não se pode
// mostrar para o lojista nem agrupar por motivo.
type UndeliverableReason string

const (
	// ReasonCommentWindowExpired — o comentário do comprador passou dos 7 dias
	// do private reply. É o caso que a campanha longa cria: a mensagem de
	// maior conversão dispara dias depois do último comentário dele.
	ReasonCommentWindowExpired UndeliverableReason = "comment_window_expired"
	// ReasonNoEligibleComment — não há comentário respondível (comprou por
	// story/DM, ou o comentário foi apagado). Sem comentário, o Instagram só
	// permite responder se ELE mandar mensagem primeiro.
	ReasonNoEligibleComment UndeliverableReason = "no_eligible_comment"
	// ReasonInstagramRejected — houve tentativa e o Instagram recusou.
	ReasonInstagramRejected UndeliverableReason = "instagram_rejected"
)

// UndeliverableReasonText traduz o motivo para o texto que o painel mostra ao
// lojista (§5.7.1 do copy deck). Fica no domínio, e não no frontend, porque é
// o mesmo texto do e-mail de aviso e da lista — duas cópias divergiriam.
func UndeliverableReasonText(r UndeliverableReason) string {
	switch r {
	case ReasonCommentWindowExpired:
		return "O último comentário deste cliente tem mais de 7 dias — o prazo do Instagram para responder já passou."
	case ReasonNoEligibleComment:
		return "Este cliente não tem comentário que possa ser respondido. A conversa só pode ser retomada se ele te mandar mensagem."
	case ReasonInstagramRejected:
		return "O Instagram recusou o envio desta mensagem. Tente falar com o cliente diretamente."
	default:
		return "Não foi possível entregar esta mensagem."
	}
}

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
	CheckoutImmediate *TemplateSettings `json:"checkout_immediate"`
	ItemAdded         *TemplateSettings `json:"item_added"`
	CheckoutReminder  *TemplateSettings `json:"checkout_reminder"`

	// Gatilhos da RN-28. Ponteiros e `omitempty` como os demais: nil significa
	// "a loja nunca customizou", e o default entra por getTemplateSettings —
	// não por escrita no JSONB de toda loja existente.
	OutOfWindowScheduled    *TemplateSettings `json:"out_of_window_scheduled,omitempty"`
	OutOfWindowSessionEnded *TemplateSettings `json:"out_of_window_session_ended,omitempty"`
	OutOfWindowEventEnded   *TemplateSettings `json:"out_of_window_event_ended,omitempty"`
	EventDeadlineStarted    *TemplateSettings `json:"event_deadline_started,omitempty"`
	WaitlistUnfulfilled     *TemplateSettings `json:"waitlist_unfulfilled,omitempty"`
	WaitlistJoined          *TemplateSettings `json:"waitlist_joined,omitempty"`

	PaymentConfirmed *EmailTemplateSettings `json:"payment_confirmed,omitempty"`
	Shipped          *EmailTemplateSettings `json:"shipped,omitempty"`
	Delivered        *EmailTemplateSettings `json:"delivered,omitempty"`
	PaymentCancelled *EmailTemplateSettings `json:"payment_cancelled,omitempty"`
	PaymentRefunded  *EmailTemplateSettings `json:"payment_refunded,omitempty"`
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
		// O texto de boas-vindas não promete mais prazo: sob a RN-04 o carrinho
		// NÃO expira enquanto a campanha roda (expires_at fica NULL de
		// propósito), então o "⏰ Válido por {expira_em}" que estava aqui era
		// falso durante o evento inteiro. O que o comprador precisa entender
		// agora é o oposto: guarde o link, ele vale a campanha toda.
		CheckoutImmediate: &TemplateSettings{
			Enabled:  true,
			Template: "Olá {handle}! 🛒\n\nAnotei seu pedido de {produto} na {evento}!\n\nTotal: {total}\n\nGuarde este link — tudo que você pedir até o fim da campanha entra aqui:\n{link} 💜",
		},
		// Numa live de duas horas "novo item adicionado" bastava. Numa campanha
		// de seis dias o comprador precisa entender que o carrinho de terça é o
		// MESMO de segunda — senão ele acha que vai pagar dois fretes e desiste.
		ItemAdded: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! ➕\n\nAdicionei {produto} ao seu carrinho da {evento}.\n\nAgora são {total_itens} itens — {total}\n\nÉ tudo no mesmo carrinho e num frete só. Você finaliza quando quiser:\n{link} 💜",
		},
		CheckoutReminder: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! 🛒\n\nSeu carrinho com {total_itens} itens está esperando!\n\nTotal: {total}\n\nFinalize aqui: {link}\n\n⏰ Válido por {expira_em}",
		},
		// A entrada na fila. O texto tem três trabalhos, nessa ordem: dizer que
		// o item NÃO foi para o carrinho (antes o comprador lia "Adicionei" e
		// achava que tinha comprado), dizer que ele não precisa fazer nada — o
		// "não precisa pedir de novo" existe porque quem acha que o pedido não
		// pegou comenta de novo, e cada recomentário vira uma tentativa a mais
		// no fluxo — e só então mostrar o carrinho, que no caso parcial é o
		// único lugar onde aparece o que de fato foi levado.
		WaitlistJoined: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! ⏳\n\n{produto} entrou na fila de espera — o estoque acabou antes de fechar seu pedido.\n\nAssim que liberar eu coloco no seu carrinho automaticamente. Não precisa pedir de novo — é só ficar de olho no seu carrinho pelo link.\n\nCarrinho: {total_itens} itens — {total}\n{link} 💜",
		},
		OutOfWindowScheduled: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! 🗓️\n\nA {evento} ainda não começou.\n\nEla abre em {comeca_em} — volte lá pra garantir o seu! 💜",
		},
		OutOfWindowSessionEnded: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! ⏱️\n\nA {sessao} já encerrou, então não consegui anotar esse pedido por ali.\n\nMas a {evento} continua! Fique de olho nas próximas publicações que eu separo pra você. 💜",
		},
		OutOfWindowEventEnded: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! 😕\n\nA {evento} já foi encerrada e não estou mais anotando pedidos por aqui.\n\nFique de olho que logo teremos novidades na {loja}! 💜",
		},
		EventDeadlineStarted: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! 🛍️\n\nA {evento} encerrou e seu carrinho está fechado com {total_itens} itens.\n\nTotal: {total}\n\nVocê tem até {prazo_final} para finalizar — depois disso os produtos voltam pra loja.\n\nFinaliza aqui: {link} 💜",
		},
		WaitlistUnfulfilled: &TemplateSettings{
			Enabled:  true,
			Template: "Oi {handle}! 😔\n\nNão consegui liberar {produto} pra você — quem estava na frente na fila acabou levando.\n\nO resto do seu carrinho da {evento} continua valendo:\n{link} 💜",
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
	// LiveTitulo é o nome do EVENTO/campanha. Alimenta {evento} (RN-19) e
	// {live_titulo}, que continua respondendo como alias depreciado: templates
	// já salvos por lojistas usam o nome antigo, e trocar a chave sem alias
	// renderizaria o placeholder cru na DM do comprador.
	LiveTitulo string

	// Sessao — nome/rótulo da transmissão. Só faz sentido no gatilho de sessão
	// encerrada, o único em que a campanha continua vendendo.
	Sessao string
	// PrazoFinal — data/hora limite para finalizar, já formatada em BRT.
	PrazoFinal string
	// ComecaEm — quando a campanha abre, já formatada em BRT.
	ComecaEm string
	// TempoExtra — prazo extra ganho por quem estava na fila ("30 minutos").
	TempoExtra string

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
	NumeroPedido   string // Order short_id, formatted as "1234"
	TrackingCode   string // Carrier tracking code, when known
	Transportadora string // Carrier name + service ("Sedex via Correios")
	LinkPedido     string // Public order page URL with tracking_token
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
	// UndeliveredReason preenchido só quando Status == StatusUndelivered.
	UndeliveredReason UndeliverableReason
	CreatedAt         time.Time
	SentAt            *time.Time
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

	// CommentCreatedAt é o instante do comentário que seria respondido, quando
	// conhecido. Zero = desconhecido, e nesse caso Send TENTA — silenciar por
	// falta de dado seria pior do que uma chamada perdida. Quando conhecido e
	// fora dos 7 dias do private reply, Send não tenta: registra undelivered
	// com motivo (RN-38). Fingir entrega é o que a regra proíbe.
	CommentCreatedAt time.Time

	// DirectOnly marca a entrega que NÃO tem comentário para responder —
	// story/DM. Sem isto, um envio sem PlatformCommentID é indistinguível de
	// "tinha comentário e a gente perdeu", e o motivo registrado mentiria.
	DirectOnly bool
}

// SendResult represents the result of a notification send attempt.
type SendResult struct {
	LogID       string
	Status      NotificationStatus
	MessageText string
	Error       error
	// Reason preenchido quando Status == StatusUndelivered.
	Reason UndeliverableReason
}

// PrivateReplyWindow é o limite do private reply do Instagram: 7 dias a contar
// do comentário, e uma única vez por comentário. Vive aqui, no domínio da
// notificação, porque é ele que decide entre "tenta" e "registra não entregue"
// — a ingestão tem a sua própria cópia do mesmo número para descartar o envio
// antes de montar payload, e as duas TÊM de concordar.
const PrivateReplyWindow = 7 * 24 * time.Hour

// Instagram DM limit is 1000 bytes
const MaxMessageBytes = 1000

package providers

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrProvenUndelivered marca um erro em que a requisição COMPROVADAMENTE não
// foi aplicada pelo provedor: falha de discagem (conexão recusada, DNS, rede
// inalcançável — nenhum byte chegou à aplicação) ou recusa de validação 4xx (o
// provedor processou e rejeitou antes de aplicar).
//
// A distinção existe porque, em movimentação de estoque, repetir é seguro
// EXATAMENTE quando este sentinela está presente. Timeout e 5xx ficam de fora
// de propósito: o provedor pode ter aplicado e falhado só em responder — na
// live de 17/08/2026 dois timeouts idênticos tiveram desfechos opostos.
var ErrProvenUndelivered = errors.New("erp request provably not applied")

// ErrOperationNotSupported is returned by providers that do not implement a
// specific capability (for example, a carrier aggregator that only quotes but
// cannot create shipments). Callers should match against this sentinel rather
// than parsing error strings.
var ErrOperationNotSupported = errors.New("operation not supported by this provider")

// ErrPublishOutcomeUnknown marca uma publicação de mídia cujo desfecho o
// provedor NÃO conseguiu determinar: a chamada de publicação falhou E a
// verificação posterior (status do container) também não respondeu. É o
// análogo social do "unconfirmed" do ledger de estoque — em 19/08/2026 um
// timeout no media_publish ENTROU (o story foi publicado) e o retry às cegas
// criou um segundo story idêntico.
//
// Quem recebe este sentinela nunca deve publicar de novo às cegas; o retry
// correto retoma o MESMO container (ResumeContainerPublish).
var ErrPublishOutcomeUnknown = errors.New("media publish outcome unknown")

// ErrContainerDead marca um container de publicação comprovadamente
// impublicável (ERROR/EXPIRED na Graph). Ao contrário do desfecho
// desconhecido, aqui é seguro criar um container novo.
var ErrContainerDead = errors.New("media container is dead")

// PublishOutcomeUnknownError carrega o container da tentativa de desfecho
// desconhecido, para que o registro de idempotência guarde a referência e o
// retry consiga retomá-lo em vez de duplicar a mídia.
type PublishOutcomeUnknownError struct {
	ContainerID string
	Err         error
}

func (e *PublishOutcomeUnknownError) Error() string {
	return "media publish outcome unknown (container " + e.ContainerID + "): " + e.Err.Error()
}

func (e *PublishOutcomeUnknownError) Unwrap() error { return e.Err }

// Is faz errors.Is(err, ErrPublishOutcomeUnknown) funcionar sem perder a
// causa original na cadeia de Unwrap.
func (e *PublishOutcomeUnknownError) Is(target error) bool {
	return target == ErrPublishOutcomeUnknown
}

// ProviderType represents the category of integration.
type ProviderType string

const (
	ProviderTypePayment       ProviderType = "payment"
	ProviderTypeERP           ProviderType = "erp"
	ProviderTypeSocial        ProviderType = "social"
	ProviderTypeShipping      ProviderType = "shipping"
	ProviderTypeCommunication ProviderType = "communication"
)

// ProviderName represents a specific integration provider.
type ProviderName string

const (
	ProviderMercadoPago ProviderName = "mercado_pago"
	ProviderPagarme     ProviderName = "pagarme"
	ProviderTiny        ProviderName = "tiny"
	ProviderBling       ProviderName = "bling"
	ProviderInstagram   ProviderName = "instagram"
	ProviderMelhorEnvio ProviderName = "melhor_envio"
	ProviderSmartEnvios ProviderName = "smartenvios"

	ProviderTwilioWhatsApp ProviderName = "twilio_whatsapp"
)

// Credentials holds authentication data for providers.
// Stored encrypted in the database.
type Credentials struct {
	// OAuth2 credentials
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`

	// API Key credentials (for non-OAuth providers like Tiny)
	APIKey    string `json:"api_key,omitempty"`
	APISecret string `json:"api_secret,omitempty"`

	// Provider-specific extra data
	Extra map[string]any `json:"extra,omitempty"`
}

// IsExpired checks if OAuth credentials are expired or about to expire.
func (c *Credentials) IsExpired() bool {
	if c.ExpiresAt.IsZero() {
		return false // Non-expiring credentials
	}
	return time.Now().Add(5 * time.Minute).After(c.ExpiresAt)
}

// Provider is the base interface all providers must implement.
type Provider interface {
	// Type returns the provider type (payment, erp).
	Type() ProviderType

	// Name returns the provider name (mercado_pago, tiny).
	Name() ProviderName

	// ValidateCredentials checks if the current credentials are valid.
	ValidateCredentials(ctx context.Context) error

	// RefreshToken refreshes OAuth tokens if applicable.
	// Returns nil if the provider doesn't use OAuth or token refresh is not needed.
	RefreshToken(ctx context.Context) (*Credentials, error)

	// TestConnection tests the connection to the provider.
	// Returns detailed information about the connection status.
	TestConnection(ctx context.Context) (*TestConnectionResult, error)
}

// TestConnectionResult contains the result of a connection test.
type TestConnectionResult struct {
	Success     bool           `json:"success"`
	Message     string         `json:"message"`
	Latency     time.Duration  `json:"latency_ms"`
	AccountInfo map[string]any `json:"account_info,omitempty"` // Provider-specific account details
	TestedAt    time.Time      `json:"tested_at"`
}

// PaymentProvider interface for payment gateway integrations.
type PaymentProvider interface {
	Provider

	// CreateCheckout creates a payment checkout session.
	CreateCheckout(ctx context.Context, order CheckoutOrder) (*CheckoutResult, error)

	// GetPaymentStatus retrieves the current status of a payment.
	GetPaymentStatus(ctx context.Context, paymentID string) (*PaymentStatus, error)

	// RefundPayment initiates a refund for a payment.
	// If amount is nil, performs a full refund.
	RefundPayment(ctx context.Context, paymentID string, amount *int64) (*RefundResult, error)

	// ==========================================================================
	// TRANSPARENT CHECKOUT METHODS
	// ==========================================================================

	// GetPublicKey returns the public key for client-side SDK initialization.
	// For Mercado Pago: returns the public_key from OAuth credentials
	// For Pagar.me: returns the public_key stored in credentials
	GetPublicKey(ctx context.Context) (string, error)

	// ProcessCardPayment processes a payment with a tokenized card.
	// The card token is generated client-side using the payment provider's SDK.
	ProcessCardPayment(ctx context.Context, input CardPaymentInput) (*CardPaymentResult, error)

	// GeneratePixPayment generates a PIX QR code for payment.
	GeneratePixPayment(ctx context.Context, input PixPaymentInput) (*PixPaymentResult, error)

	// CancelPixPayment cancela uma cobranca PIX ainda nao paga, para que o QR
	// que ja esta na mao do comprador deixe de ser pagavel.
	//
	// Sem isto, mudar o carrinho depois de gerar o PIX deixa dois codigos vivos
	// e quem paga o antigo paga o valor errado. Os dois gateways aceitam:
	// Pagar.me por DELETE /charges/{id} ("a qualquer momento, desde que nao
	// tenha sido paga") e Mercado Pago por PUT /v1/payments/{id} com status
	// cancelled.
	//
	// O id e o de CANCELAMENTO, que nem sempre e o mesmo de PixPaymentResult.
	// PaymentID: no Pagar.me aquele e a ordem e este e a cobranca.
	//
	// Best-effort por natureza: se a cobranca ja foi paga o gateway recusa, e
	// recusar e a resposta certa — quem chama trata como "nao deu para
	// invalidar" e nao como falha da operacao do comprador.
	CancelPixPayment(ctx context.Context, chargeID string) error

	// GetPaymentMethods returns the available payment methods for the store.
	GetPaymentMethods(ctx context.Context) ([]string, error)
}

// ERPHealthCheckCategory groups the cadastros the merchant has to keep in
// sync with LiveCart so orders, parcelas and shipments are categorized
// correctly. The frontend renders one section per category with copy taken
// from PanelPath ("where to cadastrar in the ERP").
type ERPHealthCheckCategory string

const (
	ERPHealthFormaPagamento   ERPHealthCheckCategory = "forma_pagamento"
	ERPHealthFormaRecebimento ERPHealthCheckCategory = "forma_recebimento"
	ERPHealthFormaEnvio       ERPHealthCheckCategory = "forma_envio"
)

// ERPHealthCheckStatus reports whether a single expected cadastro is present
// in the ERP. "ok" means the lookup returned a usable ID; "missing" means
// the merchant needs to create it.
type ERPHealthCheckStatus string

const (
	ERPHealthStatusOK      ERPHealthCheckStatus = "ok"
	ERPHealthStatusMissing ERPHealthCheckStatus = "missing"
	// ERPHealthStatusUnknown: o lookup FALHOU (429/timeout/5xx) — não dá para
	// afirmar que o cadastro falta. Sem este estado, erro transitório virava
	// falsa pendência na cara do lojista (bug de campo, jul/2026).
	ERPHealthStatusUnknown ERPHealthCheckStatus = "unknown"
)

// ERPHealthCheckItem describes one expected cadastro and its current state
// in the merchant's ERP. ExpectedName is the canonical name LiveCart looks
// for (case-insensitive); MatchedID/MatchedName carry whatever the ERP
// returned when status is OK so the frontend can show "matched as ...".
type ERPHealthCheckItem struct {
	Category     ERPHealthCheckCategory `json:"category"`
	ExpectedName string                 `json:"expected_name"`
	Status       ERPHealthCheckStatus   `json:"status"`
	MatchedID    int64                  `json:"matched_id,omitempty"`
	MatchedName  string                 `json:"matched_name,omitempty"`
	Description  string                 `json:"description"`
	PanelPath    string                 `json:"panel_path"`
}

// ERPHealthCheckResult is the bundle returned by ERPHealthChecker.HealthCheck.
type ERPHealthCheckResult struct {
	CheckedAt time.Time            `json:"checked_at"`
	Items     []ERPHealthCheckItem `json:"items"`
}

// ERPHealthChecker is an optional capability some ERP providers expose to
// audit the merchant's cadastros. Providers implement it via type assertion
// rather than embedding it in ERPProvider so we don't force every adapter
// to ship a check (Bling/Omie/etc. may not expose the necessary endpoints).
type ERPHealthChecker interface {
	HealthCheck(ctx context.Context) (*ERPHealthCheckResult, error)
}

// ERPInvoiceStatus is the LiveCart-normalised state of an issued NFe across
// ERPs. The merchant emits the NFe directly in the ERP — LiveCart only
// consumes status transitions (via webhook or manual sync) so the shipping
// flow knows when a chave de acesso is ready.
type ERPInvoiceStatus string

const (
	ERPInvoiceStatusPending    ERPInvoiceStatus = "pending"
	ERPInvoiceStatusAuthorized ERPInvoiceStatus = "authorized"
	ERPInvoiceStatusCancelled  ERPInvoiceStatus = "cancelled"
	ERPInvoiceStatusRejected   ERPInvoiceStatus = "rejected"
)

// ERPInvoice is the normalised NFe view returned by ERPInvoiceProvider. Some
// fields are optional because not every ERP exposes them (e.g. XML may need
// a separate call); callers tolerate the empty string.
type ERPInvoice struct {
	InvoiceID  string           // ERP-side id (e.g. Tiny notafiscal.id)
	Number     string           // human-readable nfe number
	Series     string           // serie da nfe
	AccessKey  string           // chave de acesso (44 digits) — empty when not yet authorised
	Status     ERPInvoiceStatus // normalised across ERPs
	StatusRaw  string           // provider-specific raw value, for debugging
	IssuedAt   time.Time        // when the NFe was emitted/authorised; zero when still pending
	XMLContent []byte           // optional XML payload; ERPs that need a separate fetch leave this empty
}

// ERPInvoiceProvider is an optional capability for ERP integrations that
// expose access to the NFe issued for an order. LiveCart never triggers
// emission — the merchant always emits in the ERP's panel — but it does
// fetch the resulting chave de acesso to feed the shipping carriers.
//
// Providers implement it via type assertion so adapters that don't speak
// nota-fiscal aren't forced to ship a stub.
type ERPInvoiceProvider interface {
	// GetInvoiceByOrder returns the NFe attached to an ERP order, when one
	// exists. ErrInvoiceNotFound is returned when the order has no NFe yet.
	GetInvoiceByOrder(ctx context.Context, orderID string) (*ERPInvoice, error)

	// GetInvoiceByID fetches a specific NFe by its ERP-side id (the value
	// arriving on a webhook). Useful when the webhook payload doesn't carry
	// the chaveAcesso directly.
	GetInvoiceByID(ctx context.Context, invoiceID string) (*ERPInvoice, error)

	// GetInvoiceXML fetches the raw NFe XML so it can be uploaded to the
	// shipping carrier (when required).
	GetInvoiceXML(ctx context.Context, invoiceID string) ([]byte, error)
}

// ErrInvoiceNotFound is returned by ERPInvoiceProvider implementations when
// the order has no NFe linked yet. Callers translate this to the "aguardando
// NFe" UI state instead of surfacing a generic error.
var ErrInvoiceNotFound = errors.New("invoice not found")

// ErrOrderNotFound é o ERP dizendo que o pedido NÃO EXISTE MAIS (HTTP 404).
//
// Não é falha: é resposta. O lojista apagou o pedido no ERP, ou ele nunca foi
// desta conta. Quem lê situação usa isto para distinguir "não consegui
// perguntar" de "perguntei e não existe" — a segunda não merece erro no log,
// e a primeira sim.
var ErrOrderNotFound = errors.New("pedido não existe no ERP")

// ERPStockReader é a leitura de saldo de UM produto. Já era resolvida por type
// assertion antes de existir com nome; nomeá-la torna o contrato explícito.
type ERPStockReader interface {
	GetProductStock(ctx context.Context, productID string) (int, error)
}

// ERPStockDetailReader devolve saldo, reservado e disponível separados.
type ERPStockDetailReader interface {
	GetProductStockDetail(ctx context.Context, productID string) (ERPStockDetail, error)
}

// ERPStockBatchReader lê o saldo de VÁRIOS produtos numa requisição.
//
// É capacidade OPCIONAL porque nem todo ERP a tem: o Tiny obriga 1 GET por
// produto (e foi assim que o resync virou 12,5 req/s e fonte de 429), enquanto
// o Bling expõe GET /estoques/saldos?idsProdutos[] e resolve 300 produtos em
// ~3 requisições.
//
// Quem consome DEVE ter fallback para a leitura unitária: um adapter sem esta
// capacidade continua correto, só mais caro.
//
// Contrato: a chave do mapa é o external id do produto. Um id AUSENTE do mapa
// significa "o ERP não devolveu saldo para este produto" — nunca zero. Quem
// espelha trata ausência como "não sei" e NÃO escreve o contador local.
type ERPStockBatchReader interface {
	GetProductStockBatch(ctx context.Context, externalIDs []string) (map[string]ERPStockDetail, error)
}

// ERPProvider interface for ERP system integrations.
type ERPProvider interface {
	Provider

	// CreateOrder creates an order in the ERP for invoicing.
	CreateOrder(ctx context.Context, order ERPOrder) (*OrderResult, error)

	// ReverseOrderStock devolve o estoque lançado de um pedido
	// (POST /pedidos/{id}/estornar-estoque). É a ÚNICA operação de estoque que
	// sobrou, e existe só como recuperação: quando alguém lança o estoque à mão
	// no painel, o pedido trava para edição e só o estorno o destrava.
	//
	// 🔴 Nunca chamar de forma especulativa. Medido em 26/08/2026 numa conta com
	// o módulo de reserva ativo: num pedido que apenas RESERVOU, o estorno
	// devolve 204 e INFLA a reserva pela quantidade do pedido, a cada chamada,
	// sem teto (2 un.: reservado 5 → 7 → 9, disponível 1 → −2 → −4). Só depois
	// de a própria API responder "estoque lançado" é que ele é seguro.
	ReverseOrderStock(ctx context.Context, orderID string) error

	// ApproveOrder sets the order status to approved in the ERP.
	ApproveOrder(ctx context.Context, orderID string) error

	// UpdateOrderItems substitui a grade de itens do pedido
	// (PUT /pedidos/{id}/itens). É como um segundo comentário da mesma pessoa
	// entra na venda: a grade vai inteira, como deve ficar, e o Tiny reajusta a
	// reserva sozinho.
	//
	// Devolve ErrOrderStockLaunched quando o pedido está travado por estoque
	// lançado à mão — o único caso em que o chamador deve estornar e repetir.
	UpdateOrderItems(ctx context.Context, orderID string, items []ERPOrderItem) error

	// UpdateOrderPayment writes the real payment installments onto an existing
	// order (PUT /pedidos/{id}). No stock movement — the confirm path of the
	// order-as-reservation flow depends on that.
	UpdateOrderPayment(ctx context.Context, orderID string, payment *ERPOrderPayment) error

	// SetOrderSituacao transitions the order status (PUT /pedidos/{id}/situacao).
	// Use as constantes Situacao* — o enum completo do ERP está lá.
	SetOrderSituacao(ctx context.Context, orderID string, situacao int) error

	// SetOrderInstallments grava as parcelas do pedido EXPLICITAMENTE, uma a uma.
	//
	// Diferente de UpdateOrderPayment, que deriva as parcelas do método e do
	// número de vezes: aqui o chamador diz exatamente quanto foi pago e quanto
	// falta. É o que separa, no pedido, o dinheiro que já entrou do que ainda
	// não — ver ERPInstallment.
	SetOrderInstallments(ctx context.Context, orderID string, parcelas []ERPInstallment) error

	// GetOrderTotal lê o total atual do pedido no ERP.
	GetOrderTotal(ctx context.Context, orderID string) (cents int64, hasInvoice bool, err error)

	// GetOrderItems lê a grade atual do pedido, com a informação adicional de
	// cada linha. É o que permite preservar o que o lojista acrescentou à mão:
	// a escrita substitui a grade inteira, então sem reler antes a linha dele
	// desaparece em silêncio.
	GetOrderItems(ctx context.Context, orderID string) ([]ERPOrderItem, error)

	// GetOrderSituacao lê a situação atual do pedido no ERP
	// (GET /pedidos/{id}). É a reconciliação do rastreamento: webhook perdido
	// ou fora de ordem deixa o LiveCart mostrando um estágio que já passou.
	GetOrderSituacao(ctx context.Context, orderID string) (int, error)

	// FindOrderIDByMarker resolve o pedido pela âncora lc-cart-<cartID>. É como
	// uma tentativa que morreu depois do POST reencontra o pedido em vez de criar
	// um segundo. Devolve "" quando não existe.
	FindOrderIDByMarker(ctx context.Context, marker string) (string, error)

	// ReverseLegacyStockExit devolve ao estoque uma saída manual do modelo ANTIGO.
	// Só a drenagem única chama — ver a nota na implementação; sai junto com ela.
	ReverseLegacyStockExit(ctx context.Context, productID string, qty int, obs string) (string, error)

	// SearchContacts searches for contacts by name or document.
	SearchContacts(ctx context.Context, params SearchContactsParams) ([]ERPContactResult, error)

	// CreateContact creates a new contact in the ERP.
	CreateContact(ctx context.Context, contact ERPContactInput) (*ERPContactResult, error)

	// UpdateContact patches an existing contact with fresh customer data.
	// Best-effort caller pattern: errors are logged, not propagated.
	UpdateContact(ctx context.Context, contactID string, contact ERPContactInput) error

	// ListProducts retrieves products from the ERP.
	ListProducts(ctx context.Context, params ListProductsParams) (*ProductListResult, error)

	// GetProduct retrieves a single product by ID.
	GetProduct(ctx context.Context, productID string) (*ERPProduct, error)

	// SyncProduct updates or creates a product in the ERP.
	SyncProduct(ctx context.Context, product ERPProduct) (*SyncResult, error)
}

// SocialProvider interface for social media integrations.
type SocialProvider interface {
	Provider

	// GetProfile retrieves the connected account profile information.
	GetProfile(ctx context.Context) (*SocialProfile, error)

	// SendDirectMessage sends a text DM to the given platform user.
	// Note: Subject to 24h messaging window restriction.
	SendDirectMessage(ctx context.Context, recipientID, text string) error

	// ReplyToComment replies to a comment (live or post) publicly.
	// This method does NOT have the 24h messaging window restriction.
	ReplyToComment(ctx context.Context, commentID, text string) error

	// SendPrivateReply sends a private DM to the user who made a comment.
	// This uses the Private Reply feature - sends a DM in response to a comment.
	// Unlike ReplyToComment (public), this sends a private message to the commenter.
	SendPrivateReply(ctx context.Context, commentID, text string) error

	// GetActiveLives retrieves all live videos currently being broadcast.
	// Only returns lives that are actively streaming at the time of the request.
	GetActiveLives(ctx context.Context) ([]LiveMedia, error)

	// HideComment hides or unhides a comment. Instagram has no edit-text
	// endpoint, so hide/unhide is the supported "update" moderation action.
	HideComment(ctx context.Context, commentID string, hidden bool) error

	// DeleteComment deletes a comment owned by the connected account.
	DeleteComment(ctx context.Context, commentID string) error

	// GetUserMedia lists recent published posts/reels (newest first) for the post
	// selector. `after` is the paging cursor ("" for the first page).
	GetUserMedia(ctx context.Context, limit int, after string) (*MediaPage, error)

	// GetMediaComments lists top-level comments on a media object (used by the
	// polling capture for post-commerce events).
	GetMediaComments(ctx context.Context, mediaID string) ([]MediaComment, error)

	// PublishImagePost creates and publishes an image feed post from a public
	// JPEG URL, returning the published media id.
	PublishImagePost(ctx context.Context, imageURL, caption string) (string, error)

	// PublishReel publishes a Reel from a public video URL (graph.instagram.com
	// requires video_url), returning the published media id.
	PublishReel(ctx context.Context, videoURL, caption string) (string, error)

	// PublishStory publishes a Story (media_type=STORIES) from a public photo or
	// video URL (isVideo selects image_url vs video_url), returning the media id.
	PublishStory(ctx context.Context, mediaURL string, isVideo bool) (string, error)

	// GetUsername resolves a user's @handle from their Instagram-scoped id
	// (IGSID). Best-effort — returns "" when the lookup isn't permitted.
	GetUsername(ctx context.Context, igsid string) (string, error)

	// GetMediaDetails fetches metadata (permalink, thumbnail, caption) for a media id.
	GetMediaDetails(ctx context.Context, mediaID string) (*MediaPost, error)
}

// MediaComment is a top-level comment on a media object as returned by
// GET /{ig-media-id}/comments.
type MediaComment struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
	Username  string `json:"username,omitempty"`
	From      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
}

// SocialProfile contains social media account information.
type SocialProfile struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
}

// =============================================================================
// COMMUNICATION PROVIDER (WhatsApp — PRD 006)
// =============================================================================

// CommunicationProvider interface for messaging integrations (e.g. WhatsApp
// via Twilio). Business-initiated messages require a pre-approved template.
type CommunicationProvider interface {
	Provider

	// SendTemplateMessage sends a pre-approved template message to a phone
	// number. Returns the provider message ID for status tracking.
	SendTemplateMessage(ctx context.Context, msg TemplateMessage) (*MessageResult, error)

	// GetSenderStatus returns the registration/health status of the WhatsApp
	// sender (the merchant's phone number).
	GetSenderStatus(ctx context.Context) (*SenderStatus, error)
}

// TemplateMessage is a business-initiated WhatsApp template send.
type TemplateMessage struct {
	To          string            // destination in E.164 (+5511999999999)
	ContentSid  string            // approved content/template ID at the provider
	Variables   map[string]string // template variables, keyed "1", "2", ...
	CallbackURL string            // per-message status callback URL (optional)
}

// MessageResult is the immediate result of a message send.
type MessageResult struct {
	MessageID string // provider message SID (e.g. Twilio "SM...")
	Status    string // queued | sent | ...
}

// SenderStatus describes the WhatsApp sender (merchant number) state.
type SenderStatus struct {
	Status        string `json:"status"`         // ONLINE | PENDING_VERIFICATION | OFFLINE | ...
	PhoneNumber   string `json:"phone_number"`   // E.164
	QualityRating string `json:"quality_rating"` // HIGH | MEDIUM | LOW | UNKNOWN
}

// LiveMedia represents a live video on a social platform.
type LiveMedia struct {
	ID               string `json:"id"`
	MediaType        string `json:"media_type"`
	MediaProductType string `json:"media_product_type"`
	Username         string `json:"username"`
	Timestamp        string `json:"timestamp,omitempty"`
}

// MediaPage is a page of media with the cursor for the next page.
type MediaPage struct {
	Posts []MediaPost `json:"posts"`
	After string      `json:"after"` // next-page cursor; empty when there are no more
}

// MediaPost is a published feed post/reel used as the target of a post-commerce event.
type MediaPost struct {
	ID            string `json:"id"`
	Caption       string `json:"caption,omitempty"`
	MediaType     string `json:"media_type"`
	MediaURL      string `json:"media_url,omitempty"`
	ThumbnailURL  string `json:"thumbnail_url,omitempty"`
	Permalink     string `json:"permalink,omitempty"`
	Timestamp     string `json:"timestamp,omitempty"`
	CommentsCount int    `json:"comments_count,omitempty"`
}

// WebhookHandler interface for providers that support webhooks.
type WebhookHandler interface {
	// VerifySignature validates the webhook signature.
	VerifySignature(payload []byte, signature string, secret string) bool

	// ParseEvent parses the webhook payload into a typed event.
	ParseEvent(payload []byte) (*WebhookEvent, error)

	// HandleEvent processes a webhook event.
	HandleEvent(ctx context.Context, event *WebhookEvent) error
}

// =============================================================================
// PAYMENT TYPES
// =============================================================================

// CheckoutOrder represents an order to be paid.
type CheckoutOrder struct {
	ExternalID  string           `json:"external_id"` // Your internal order/cart ID
	Items       []CheckoutItem   `json:"items"`
	Customer    CheckoutCustomer `json:"customer"`
	TotalAmount int64            `json:"total_amount"` // In cents
	Currency    string           `json:"currency"`     // BRL, USD, etc.
	NotifyURL   string           `json:"notify_url"`   // Webhook URL for payment notifications
	SuccessURL  string           `json:"success_url"`  // Redirect URL on success
	FailureURL  string           `json:"failure_url"`  // Redirect URL on failure
	ExpiresIn   *time.Duration   `json:"expires_in,omitempty"`
	Metadata    map[string]any   `json:"metadata,omitempty"`
}

// CheckoutItem represents an item in the checkout.
type CheckoutItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Quantity    int    `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"` // In cents
	ImageURL    string `json:"image_url,omitempty"`
}

// CheckoutCustomer represents the customer paying.
type CheckoutCustomer struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Phone    string `json:"phone,omitempty"`
	Document string `json:"document,omitempty"` // CPF/CNPJ
	// Address is the shipping address captured at checkout. Optional —
	// Checkout Pro flows don't have it yet (buyer fills the form on MP's
	// hosted page) but Card / PIX flows do, and surfacing it as payer.address
	// lifts the MP fraud-screen approval rate.
	Address *CheckoutAddress `json:"address,omitempty"`
}

// CheckoutAddress is the minimal shape MP's payer.address expects. Provider
// implementations are expected to map this to whatever the upstream API
// requires (e.g., Mercado Pago's street_name / street_number / zip_code).
type CheckoutAddress struct {
	ZipCode      string `json:"zip_code"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
}

// CheckoutResult is the result of creating a checkout.
type CheckoutResult struct {
	CheckoutID  string     `json:"checkout_id"`
	CheckoutURL string     `json:"checkout_url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// PaymentStatus represents the current state of a payment.
type PaymentStatus struct {
	PaymentID         string         `json:"payment_id"`
	Status            PaymentState   `json:"status"`
	Amount            int64          `json:"amount"`
	PaidAt            *time.Time     `json:"paid_at,omitempty"`
	RefundedAt        *time.Time     `json:"refunded_at,omitempty"`
	FailureReason     string         `json:"failure_reason,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	ExternalReference string         `json:"external_reference,omitempty"` // Cart ID or order reference
	PaymentMethod     string         `json:"payment_method,omitempty"`     // pix, credit_card, debit_card, boleto
	Installments      int            `json:"installments,omitempty"`
	// MoneyReleaseDate is when the gateway will actually credit the merchant.
	// Mercado Pago surfaces this directly in /v1/payments/{id} — D+1 when the
	// merchant has antecipation enabled, D+30 otherwise — so the ERP can use
	// it as the real first-parcela due date instead of guessing.
	MoneyReleaseDate *time.Time `json:"money_release_date,omitempty"`

	// FeeAmountCents is the total fee charged to the merchant by the gateway
	// (sum of fee_details where fee_payer == "collector"), in cents. Captured
	// for audit / reconciliation — does NOT change parcela.valor today
	// (parcela still goes gross). 0 when the gateway didn't return fees yet
	// (e.g. payment still pending).
	FeeAmountCents int64 `json:"fee_amount_cents,omitempty"`
	// NetAmountCents is what the merchant will actually receive after gateway
	// fees, in cents (Mercado Pago's transaction_details.net_received_amount).
	// Equals Amount - FeeAmountCents when fees are present.
	NetAmountCents int64 `json:"net_amount_cents,omitempty"`
	// Fees is the breakdown of every fee line the gateway returned (type,
	// payer, amount in cents). Useful when more than one fee type applies
	// (mercadopago_fee, financing_fee, application_fee for marketplaces).
	Fees []PaymentFee `json:"fees,omitempty"`
}

// PaymentFee is a single fee line returned by the payment gateway.
type PaymentFee struct {
	Type        string `json:"type"`      // mercadopago_fee, financing_fee, application_fee, ...
	FeePayer    string `json:"fee_payer"` // collector, payer
	AmountCents int64  `json:"amount_cents"`
}

// PaymentState represents payment status values.
type PaymentState string

const (
	PaymentPending   PaymentState = "pending"
	PaymentApproved  PaymentState = "approved"
	PaymentRejected  PaymentState = "rejected"
	PaymentRefunded  PaymentState = "refunded"
	PaymentCancelled PaymentState = "cancelled"
	PaymentInProcess PaymentState = "in_process"
)

// RefundResult is the result of a refund operation.
type RefundResult struct {
	RefundID  string    `json:"refund_id"`
	Status    string    `json:"status"`
	Amount    int64     `json:"amount"`
	CreatedAt time.Time `json:"created_at"`
}

// =============================================================================
// TRANSPARENT CHECKOUT TYPES
// =============================================================================

// CardPaymentInput contains the input for processing a card payment.
type CardPaymentInput struct {
	// CartID is the internal cart identifier
	CartID string `json:"cart_id"`

	// Token is the card token generated by the payment provider's SDK
	Token string `json:"token"`

	// Installments is the number of installments (1 for full payment)
	Installments int `json:"installments"`

	// Customer information
	Customer CheckoutCustomer `json:"customer"`

	// Items in the cart
	Items []CheckoutItem `json:"items"`

	// TotalAmount is the total payment amount in cents
	TotalAmount int64 `json:"total_amount"`

	// Currency code (BRL, USD, etc.)
	Currency string `json:"currency"`

	// NotifyURL is the webhook URL for payment notifications
	NotifyURL string `json:"notify_url"`

	// Metadata for additional context
	Metadata map[string]any `json:"metadata,omitempty"`

	// DeviceID for fraud prevention (optional, provider-specific).
	// For Pagar.me this is forwarded as the order-level `session_id` (the
	// device-fingerprint id their antifraud correlates against); for Mercado
	// Pago it becomes the X-meli-session-id header.
	DeviceID string `json:"device_id,omitempty"`

	// IPAddress is the buyer's real client IP, captured server-side from the
	// checkout request (X-Forwarded-For behind the proxy). Pagar.me's antifraud
	// weighs it heavily — omitting it makes every transaction look like it
	// comes from an unknown origin and inflates the risk score.
	IPAddress string `json:"ip_address,omitempty"`

	// PayerCost contains installment info from Mercado Pago SDK (optional)
	PayerCost *PayerCostInfo `json:"payer_cost,omitempty"`

	// PaymentMethodID for Mercado Pago (visa, master, etc.)
	PaymentMethodID string `json:"payment_method_id,omitempty"`

	// IssuerId for Mercado Pago
	IssuerID string `json:"issuer_id,omitempty"`
}

// PayerCostInfo contains installment cost information from Mercado Pago SDK.
type PayerCostInfo struct {
	Installments    int     `json:"installments"`
	InstallmentRate float64 `json:"installment_rate"`
	TotalAmount     float64 `json:"total_amount"`
}

// CardPaymentResult is the result of processing a card payment.
type CardPaymentResult struct {
	// PaymentID is the provider's payment identifier
	PaymentID string `json:"payment_id"`

	// Status is the payment status (approved, rejected, pending, in_process)
	Status PaymentState `json:"status"`

	// StatusDetail provides more info about the status (e.g., "accredited", "cc_rejected_other_reason")
	StatusDetail string `json:"status_detail,omitempty"`

	// Amount paid in cents
	Amount int64 `json:"amount"`

	// Installments used
	Installments int `json:"installments"`

	// Last four digits of the card
	LastFourDigits string `json:"last_four_digits,omitempty"`

	// Card brand (visa, master, etc.)
	CardBrand string `json:"card_brand,omitempty"`

	// AuthorizationCode is the NSU / acquirer auth code returned by the
	// adquirente. Empty when the gateway does not surface it (still pending,
	// rejected, or not present in this provider's payload).
	AuthorizationCode string `json:"authorization_code,omitempty"`

	// PaidAt is the moment the gateway recorded the authorization. The
	// gateway is the source of truth here — using the server clock would
	// drift in either direction (server skew, retries from queues, etc.).
	// Nil when the response does not carry a paid_at (still pending /
	// rejected / provider omitted the field).
	PaidAt *time.Time `json:"paid_at,omitempty"`

	// ExternalReference is the cart ID or order reference
	ExternalReference string `json:"external_reference,omitempty"`

	// Message for the user
	Message string `json:"message,omitempty"`
}

// PixPaymentInput contains the input for generating a PIX payment.
type PixPaymentInput struct {
	// CartID is the internal cart identifier
	CartID string `json:"cart_id"`

	// Customer information
	Customer CheckoutCustomer `json:"customer"`

	// Items in the cart
	Items []CheckoutItem `json:"items"`

	// TotalAmount is the total payment amount in cents
	TotalAmount int64 `json:"total_amount"`

	// Currency code (BRL)
	Currency string `json:"currency"`

	// NotifyURL is the webhook URL for payment notifications
	NotifyURL string `json:"notify_url"`

	// ExpiresIn is how long the PIX code is valid (default: 30 minutes)
	ExpiresIn *time.Duration `json:"expires_in,omitempty"`

	// Metadata for additional context
	Metadata map[string]any `json:"metadata,omitempty"`
}

// PixPaymentResult is the result of generating a PIX payment.
type PixPaymentResult struct {
	// PaymentID is the provider's payment identifier
	PaymentID string `json:"payment_id"`

	// CancelID identifica a cobranca para efeito de CANCELAMENTO.
	//
	// No Pagar.me PaymentID e a ordem (`or_...`) e o cancelamento exige a
	// cobranca (`ch_...`). No Mercado Pago os dois coincidem. Quem cancela usa
	// este campo e nao precisa saber de qual provedor veio.
	CancelID string `json:"cancel_id"`

	// Status is the initial payment status (always pending for PIX)
	Status PaymentState `json:"status"`

	// QRCode is the PIX QR code as base64 image
	QRCode string `json:"qr_code"`

	// QRCodeText is the PIX copy-paste code (copia e cola)
	QRCodeText string `json:"qr_code_text"`

	// Amount in cents
	Amount int64 `json:"amount"`

	// ExpiresAt is when the PIX code expires
	ExpiresAt time.Time `json:"expires_at"`

	// ExternalReference is the cart ID or order reference
	ExternalReference string `json:"external_reference,omitempty"`

	// TicketURL is an optional URL to view the payment (Mercado Pago)
	TicketURL string `json:"ticket_url,omitempty"`
}

// CheckoutConfigResult contains the checkout configuration for the frontend.
type CheckoutConfigResult struct {
	// Provider name (mercado_pago, pagarme)
	Provider ProviderName `json:"provider"`

	// PublicKey for SDK initialization
	PublicKey string `json:"public_key"`

	// AvailableMethods lists the payment methods available (card, pix)
	AvailableMethods []string `json:"available_methods"`
}

// =============================================================================
// ERP TYPES
// =============================================================================

// ERPOrder represents an order to create in the ERP.
type ERPOrder struct {
	ExternalID  string         `json:"external_id"` // Your internal order/cart ID
	ContactID   string         `json:"contact_id"`  // ERP contact ID (required for Tiny v3)
	Items       []ERPOrderItem `json:"items"`
	TotalAmount int64          `json:"total_amount"` // In cents (includes shipping when present)
	Observation string         `json:"observation,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`

	// ShippingAddress is the delivery address. When set, the provider ships it
	// as enderecoEntrega (or equivalent) on the order.
	ShippingAddress *ERPShippingAddress `json:"shipping_address,omitempty"`

	// Shipping, when set, feeds the provider with the carrier/service chosen
	// by the customer and the freight value so the ERP records the shipment.
	Shipping *ERPOrderShipping `json:"shipping,omitempty"`

	// Payment, when set, carries the FINANCIAL entry: the provider fills
	// parcelas with dataPagamento and records the payment method/ID in the ERP.
	//
	// Absent does NOT mean unpaid — see Approve. A sale paid outside LiveCart is
	// paid, but its receivable is entered by the merchant in the ERP, with the
	// form the money actually arrived in.
	Payment *ERPOrderPayment `json:"payment,omitempty"`

	// Approve asks the provider to leave the order APPROVED (Tiny situação 3)
	// instead of open.
	//
	// Separate from Payment on purpose. Approval answers "is this sale closed?";
	// Payment answers "who records the receivable?". They travelled together
	// while every payment came from the gateway, and using one to decide the
	// other left a manually-paid order sitting as "Em aberto" for the merchant
	// to approve by hand — the same state the failed orders of 16/08 ended in.
	//
	// False on the pre-payment conversion: that order exists to hold the grid,
	// and approving it would register a sale nobody paid for.
	Approve bool `json:"approve,omitempty"`
}

// StorePickupCarrier is the Carrier the checkout writes when the buyer chose
// "retirar na loja". It lives here, and not as a literal on each side, because
// the ERP layer has to recognise it: retirada não é uma remessa, e tratá-la
// como transportadora fez o Tiny recusar o pedido inteiro.
const StorePickupCarrier = "Retirada na loja"

// ERPOrderShipping captures the freight option chosen at checkout so the ERP
// records the shipment alongside the sales order.
type ERPOrderShipping struct {
	Carrier      string `json:"carrier"`                 // "Correios", "Jadlog"...
	Service      string `json:"service"`                 // "PAC", "SEDEX", ".Package"...
	CostCents    int64  `json:"cost_cents"`              // actual merchant cost (real quote value)
	DeadlineDays int    `json:"deadline_days,omitempty"` // estimated max delivery time
}

// ERPShippingAddress describes a delivery address for an ERP order.
type ERPShippingAddress struct {
	RecipientName string `json:"recipient_name,omitempty"` // nomeDestinatario
	Document      string `json:"document,omitempty"`       // cpfCnpj
	Street        string `json:"street"`                   // endereco
	Number        string `json:"number"`                   // enderecoNro
	Complement    string `json:"complement,omitempty"`     // complemento
	Neighborhood  string `json:"neighborhood"`             // bairro
	City          string `json:"city"`                     // municipio
	State         string `json:"state"`                    // uf (2 chars)
	ZipCode       string `json:"zip_code"`                 // cep
	Phone         string `json:"phone,omitempty"`          // fone
}

// ERPOrderPayment captures the payment confirmation details so the provider
// can register the order as paid (e.g. Tiny parcelas with dataPagamento).
type ERPOrderPayment struct {
	Method       string    `json:"method"`     // pix, credit_card, debit_card, boleto
	PaymentID    string    `json:"payment_id"` // gateway payment ID
	Installments int       `json:"installments,omitempty"`
	PaidAt       time.Time `json:"paid_at"`
	Amount       int64     `json:"amount"` // paid amount, in cents (usually == TotalAmount). Always GROSS — fees live in FeeAmountCents.
	// MoneyReleaseDate is when the gateway tells us it will credit the
	// merchant for the first installment — populated by MP, used by the ERP
	// adapter as the base date for parcela 1 so contas a receber matches the
	// merchant's actual repasse calendar instead of a hardcoded D+30.
	MoneyReleaseDate *time.Time `json:"money_release_date,omitempty"`

	// FeeAmountCents and NetAmountCents propagate the gross/fee/net split
	// from the payment gateway so the ERP adapter can either record the fee
	// alongside the parcela (e.g. Tiny `descontos` or `observacoes`) or
	// expose it for reconciliation reports. Today the Tiny adapter logs
	// these but does NOT subtract fees from parcela.valor — the parcela
	// stays at the gross Amount, matching the NF-e value.
	FeeAmountCents int64 `json:"fee_amount_cents,omitempty"`
	NetAmountCents int64 `json:"net_amount_cents,omitempty"`
}

// SearchContactsParams contains parameters for searching contacts.
type SearchContactsParams struct {
	Name    string `json:"name,omitempty"`
	CpfCnpj string `json:"cpf_cnpj,omitempty"`
}

// ERPContactInput represents data for creating a contact in the ERP.
type ERPContactInput struct {
	Name       string `json:"name"`
	CpfCnpj    string `json:"cpf_cnpj,omitempty"`
	Email      string `json:"email,omitempty"`
	Phone      string `json:"phone,omitempty"`
	PersonType string `json:"person_type,omitempty"` // "F" = Física, "J" = Jurídica
}

// ERPContactResult is the result of searching or creating a contact.
type ERPContactResult struct {
	ContactID string `json:"contact_id"`
	Name      string `json:"name"`
}

// ERPOrderItem represents an item in an ERP order.
type ERPOrderItem struct {
	ProductID string `json:"product_id"` // ERP product ID
	SKU       string `json:"sku,omitempty"`
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
	UnitPrice int64  `json:"unit_price"` // In cents
	// Note vai para a informação adicional da linha no ERP e volta intacta na
	// leitura. É por ela que se sabe, ao reler um pedido, quais linhas são
	// nossas e quais o lojista digitou à mão — ver LiveCartItemMarker.
	Note string `json:"note,omitempty"`
}

// ERPInstallment é uma parcela do pedido, dita por extenso.
//
// 🔴 A soma das parcelas é FORÇADA ao total do pedido, em silêncio. Medido em
// 26/08/2026: enviar uma parcela de R$ 60 num pedido de R$ 100 grava R$ 100, com
// HTTP 204 e sem aviso. Quem manda um valor que não fecha não recebe erro —
// recebe outro número.
//
// Daí este tipo existir. Acrescentar item a um pedido PAGO faz o ERP
// redistribuir o total pelas parcelas existentes: uma venda de R$ 40 que virou
// R$ 145 passou a registrar que a compradora pagou R$ 145. Um terceiro item
// dividiu R$ 195 em duas parcelas de R$ 97,50. O registro financeiro é destruído
// a cada edição, e só reenviar a divisão correta o reconstrói.
type ERPInstallment struct {
	// AmountCents é o valor desta parcela. A soma de todas TEM de dar o total do
	// pedido, ou o ERP a reescreve sozinho.
	AmountCents int64
	// DueDate é o vencimento. Para a parcela já paga, é a data do pagamento.
	DueDate time.Time
	// Note é o que o lojista lê no painel — é aqui que "PAGO" e "A PAGAR" ficam
	// visíveis para ele.
	Note string
	// Method é COMO esta parcela foi paga: "pix", "credit_card", "debit_card",
	// "boleto". Vazio significa "não sei" — e não "dinheiro".
	//
	// Vive na PARCELA, e não na chamada, porque um carrinho pode ter mais de um
	// pagamento (PIX + cartão) e as linhas "DESCONTO concedido" e "A PAGAR" não
	// têm método nenhum. Resolver um método por chamada carimbaria o da
	// primeira parcela em todas.
	//
	// Campo ADITIVO de propósito: SetOrderInstallments não muda de assinatura.
	// internal/erp/parcelas.go faz uma asserção estrutural ANÔNIMA com a
	// assinatura exata — mudá-la compila, o `ok` vira false, e a recomposição
	// das parcelas para de acontecer EM SILÊNCIO nos dois ERPs.
	Method string
}

// LiveCartItemMarker abre a informação adicional de toda linha que o LiveCart
// escreve num pedido do ERP.
//
// Existe porque `PUT /pedidos/{id}/itens` SUBSTITUI a grade inteira. Medido em
// 26/08/2026: o lojista acrescentou um produto ao pedido pelo painel, o
// comentário seguinte da compradora fez o LiveCart reenviar a sua grade, e a
// linha dele sumiu — HTTP 204, sem aviso — junto com as 3 unidades que ela
// segurava, que voltaram à venda.
//
// Com o marcador, reler o pedido antes de escrever separa o que é nosso do que é
// dele, e o que é dele é reenviado junto. O texto é legível de propósito: o
// lojista vê "[livecart]" na linha e entende de onde ela veio.
const LiveCartItemMarker = "[livecart]"

// ErrPedidoComNotaFiscal é a recusa em mexer num pedido que já virou documento
// fiscal, decidida pelo sinal AUTORITATIVO: `idNotaFiscal != 0`.
//
// A situação não serve para isso, e a medição de 26/08/2026 mostra por quê:
// gerar a nota (`POST /pedidos/{id}/gerar-nota-fiscal`) leva o pedido de
// situação 0 para 4 ("Preparando envio") — NUNCA para 1 ("Faturada"). Quem
// esperasse a situação 1 deixaria a porta aberta com a nota já emitida.
var ErrPedidoComNotaFiscal = errors.New("pedido já tem nota fiscal emitida")

// ERPStockDetail são os três saldos que o ERP guarda de um produto.
//
// O LiveCart só usa `Available` para decidir o que vende. Os outros dois existem
// para DIAGNÓSTICO: `Reserved > 0` é a única evidência disponível de que o
// módulo de Reserva de Estoque está ativo na conta — `GET /depositos`, que traz
// `possuiReserva`, devolve 403 mesmo com o módulo ligado.
type ERPStockDetail struct {
	Balance   int
	Reserved  int
	Available int
}

// IsLiveCartItem diz se a linha foi escrita pelo LiveCart.
func IsLiveCartItem(note string) bool { return strings.HasPrefix(note, LiveCartItemMarker) }

// OrderResult is the result of creating an order in the ERP.
type OrderResult struct {
	OrderID     string `json:"order_id"`     // ERP order ID
	OrderNumber string `json:"order_number"` // Human-readable order number
	Status      string `json:"status"`
}

// ListProductsParams contains parameters for listing products.
type ListProductsParams struct {
	Page         int        `json:"page,omitempty"`
	PageSize     int        `json:"page_size,omitempty"`
	Search       string     `json:"search,omitempty"`
	GTIN         string     `json:"gtin,omitempty"`
	SKU          string     `json:"sku,omitempty"`
	ActiveOnly   bool       `json:"active_only,omitempty"`
	UpdatedAfter *time.Time `json:"updated_after,omitempty"`
}

// ProductListResult contains the result of listing products.
type ProductListResult struct {
	Products   []ERPProduct `json:"products"`
	TotalCount int          `json:"total_count"`
	Page       int          `json:"page"`
	PageSize   int          `json:"page_size"`
	HasMore    bool         `json:"has_more"`
}

// ERPShippingProfile carries the physical attributes the ERP exposes for a
// product / variation. All four dimensions plus weight are required for the
// LiveCart catalog to mark the product as "shippable" — partial data is not
// useful (and is rejected by the domain validation), so providers should
// return nil when the ERP did not supply the full set.
type ERPShippingProfile struct {
	WeightGrams   int    `json:"weight_grams"` // weight already converted to grams
	HeightCm      int    `json:"height_cm"`
	WidthCm       int    `json:"width_cm"`
	LengthCm      int    `json:"length_cm"`
	PackageFormat string `json:"package_format,omitempty"` // box | roll | letter
}

// ERPProduct represents a product in the ERP.
type ERPProduct struct {
	ID          string `json:"id"` // ERP product ID
	SKU         string `json:"sku,omitempty"`
	GTIN        string `json:"gtin,omitempty"` // Barcode (EAN/GTIN)
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Price       int64  `json:"price"` // In cents
	Stock       int    `json:"stock"`
	// StockKnown diz se `Stock` é um saldo DISPONÍVEL de verdade.
	//
	// Existe porque a alternativa — devolver zero, ou pior, o saldo físico —
	// é indistinguível de "esgotado" e de "tem 4". O físico está fora de
	// questão: ele conta peça já reservada por outro pedido, e vendê-la é o
	// furo que esta refatoração fecha. Então quando o disponível não pode ser
	// afirmado (produto sem controle de estoque, chamada estrangulada, resposta
	// ilegível), este campo vem falso e quem espelha NÃO escreve o contador
	// local: fica o número que já estava, que ao menos não foi inventado.
	StockKnown bool   `json:"stock_known"`
	Active     bool   `json:"active"`
	ImageURL   string `json:"image_url,omitempty"`
	// ImageURLs are ALL image URLs the ERP returned for this product (Tiny
	// anexos), in order. The merchant picks which becomes the LiveCart main
	// image on import; ImageURL stays the default (first). Empty when none.
	ImageURLs []string            `json:"image_urls,omitempty"`
	UpdatedAt time.Time           `json:"updated_at"`
	Shipping  *ERPShippingProfile `json:"shipping,omitempty"` // nil when the ERP didn't return a complete profile
	// WeightGramsHint is set whenever the ERP returned a positive weight, even
	// when dimensions are missing (so Shipping had to be nil). The integration
	// service uses it to combine with store-level default dimensions and
	// complete the profile.
	WeightGramsHint int `json:"weight_grams_hint,omitempty"`

	// Variant-related fields (populated for ERPs that expose variations like Tiny "Com Variações" / tipo=V).
	// Type carries the ERP's native product type ("S","V","K","F","M" for Tiny). Empty when unknown.
	Type             string            `json:"type,omitempty"`
	IsParent         bool              `json:"is_parent,omitempty"`          // True when this product is the aggregator (has children).
	ParentExternalID string            `json:"parent_external_id,omitempty"` // ERP id of the parent when this is a child variant.
	Attributes       map[string]string `json:"attributes,omitempty"`         // Variation grade for a child, e.g. {"Cor":"Azul","Tamanho":"M"}.
	GradeKeys        []string          `json:"grade_keys,omitempty"`         // Grade dimension names for a parent, e.g. ["Tamanho","Cor"].
	Variants         []ERPProduct      `json:"variants,omitempty"`           // Children when IsParent — populated by GetProduct.
}

// SyncResult is the result of syncing a product.
type SyncResult struct {
	ProductID string `json:"product_id"`
	Action    string `json:"action"` // created, updated, skipped
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// =============================================================================
// WEBHOOK TYPES
// =============================================================================

// WebhookEvent represents a parsed webhook event.
type WebhookEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Action    string         `json:"action,omitempty"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}

// =============================================================================
// SHIPPING TYPES
// =============================================================================

// ShippingProvider is the minimum contract all shipping integrations must
// implement. It covers quoting (checkout) and carrier listing (admin / test
// connection). Order-lifecycle operations (create shipment, invoice, labels,
// tracking) live on the optional ShippingOrderProvider extension so that
// quote-only aggregators are still valid providers.
type ShippingProvider interface {
	Provider

	// Quote calculates freight options for a shipment.
	Quote(ctx context.Context, req QuoteRequest) ([]QuoteOption, error)

	// ListCarriers returns the available carriers/services for the connected account.
	ListCarriers(ctx context.Context) ([]CarrierService, error)
}

// ShippingOrderProvider extends ShippingProvider with post-quote operations.
// Providers that implement it can: create shipments, attach/upload invoices,
// generate labels, and pull tracking history. Callers should type-assert and
// surface ErrOperationNotSupported when the provider does not implement this
// interface or when a specific call returns it.
type ShippingOrderProvider interface {
	ShippingProvider

	// CreateShipment creates a freight order at the carrier aggregator, optionally
	// tied to a prior quote (QuoteServiceID). Returns the provider's shipment
	// reference that the caller should persist.
	CreateShipment(ctx context.Context, req CreateShipmentRequest) (*CreateShipmentResult, error)

	// AttachInvoice links an already-emitted fiscal document (NFe/DCe) to an
	// existing shipment by key. Use this for async flows where the invoice is
	// emitted after the shipment is created.
	AttachInvoice(ctx context.Context, req AttachInvoiceRequest) error

	// UploadInvoiceXML uploads the XML of the fiscal document to the carrier
	// aggregator. Required when the aggregator cannot fetch the XML from the
	// SEFAZ by key alone.
	UploadInvoiceXML(ctx context.Context, req UploadInvoiceXMLRequest) error

	// GenerateLabels produces the shipping labels (PDF/ZPL/base64) for the
	// given shipments. Result contains the downloadable URL plus per-volume
	// barcodes.
	GenerateLabels(ctx context.Context, req GenerateLabelsRequest) (*GenerateLabelsResult, error)

	// TrackShipment pulls the latest tracking history for a shipment. Use as
	// fallback when webhooks are not wired up.
	TrackShipment(ctx context.Context, req TrackShipmentRequest) (*TrackShipmentResult, error)
}

// ShippingZip is a Brazilian CEP, digits only (8 chars).
type ShippingZip string

// ShippingItem represents a cart item being quoted.
type ShippingItem struct {
	ID                  string // opaque identifier returned in error messages
	Name                string // human-readable description (used when creating shipments)
	WeightGrams         int
	HeightCm            int
	WidthCm             int
	LengthCm            int
	InsuranceValueCents int64
	UnitPriceCents      int64 // unit price (used when creating shipments)
	Quantity            int
	PackageFormat       string // "box", "roll", "letter" - optional, carrier hint
}

// QuoteRequest is the input for a freight quote.
type QuoteRequest struct {
	FromZip ShippingZip
	ToZip   ShippingZip
	Items   []ShippingItem
	// ExtraPackageWeightGrams is added once to the shipment to account for
	// consolidating packaging (empty box, bubble wrap). Applied to the heaviest
	// item when the provider quotes by individual products.
	ExtraPackageWeightGrams int
	// ServiceIDs restricts the quote to a subset of services. Empty = all.
	// Opaque strings because providers use different id formats (int, UUID,
	// MongoDB ObjectId, ...).
	ServiceIDs []string
	// ExternalID is an optional caller-side correlation id (e.g. cart id)
	// forwarded to providers that support it for correlation with webhooks.
	ExternalID string
	// Options are delivery-time flags.
	Receipt bool
	OwnHand bool
}

// QuoteOption is a single carrier/service result.
type QuoteOption struct {
	// Provider is the integration name that returned this option. Required so
	// the caller can route the follow-up CreateShipment call to the right
	// provider when the store has multiple shipping integrations active.
	Provider ProviderName

	// ServiceID is the opaque, provider-specific identifier for the service.
	// Pass it back as-is when creating the shipment.
	ServiceID    string
	Service      string // "PAC", "SEDEX", ".Package", etc.
	Carrier      string // "Correios", "Jadlog", "Loggi", etc.
	CarrierLogo  string // optional URL
	PriceCents   int64  // final price in cents
	DeadlineDays int    // business days
	Available    bool
	Error        string // populated when Available is false
}

// CarrierService describes one service offered by a carrier.
type CarrierService struct {
	ServiceID   string
	Service     string
	Carrier     string
	CarrierLogo string
	// Max insurance value accepted, in cents. 0 means unlimited/unknown.
	InsuranceMaxCents int64
}

// =============================================================================
// SHIPPING ORDER LIFECYCLE TYPES
// =============================================================================

// ShippingAddress describes an address used by CreateShipment (sender/destiny).
type ShippingAddressPoint struct {
	Name         string
	Document     string // CPF/CNPJ
	ZipCode      string
	Street       string
	Number       string
	Complement   string
	Neighborhood string
	City         string
	State        string // 2-letter UF
	Phone        string
	Email        string
	Observation  string
}

// CreateShipmentRequest captures everything a provider needs to turn a quote
// into a concrete freight order.
type CreateShipmentRequest struct {
	// QuoteServiceID is the opaque id returned by Quote() for the chosen
	// carrier/service. Required — callers must not create shipments without
	// a prior quote (no auto-selection in LiveCart).
	QuoteServiceID string

	// ExternalOrderID is the caller's own order identifier (used for webhook
	// correlation and lookups).
	ExternalOrderID string

	// InvoiceKey, when present, is the NFe access key. When absent the
	// shipment is created as a Declaração de Conteúdo and the invoice can be
	// linked later via AttachInvoice / UploadInvoiceXML.
	InvoiceKey string

	Sender  ShippingAddressPoint
	Destiny ShippingAddressPoint

	// Items in the shipment. Dimensions/weight MUST be set.
	Items []ShippingItem

	// VolumeCount is the number of physical packages in the shipment.
	VolumeCount int

	// Observation is free-form text appended to the shipment record.
	Observation string
}

// CreateShipmentResult is the normalized response after creating a shipment.
type CreateShipmentResult struct {
	ProviderOrderID     string // provider's internal order id (persisted)
	ProviderOrderNumber string // human-readable order number (optional)
	TrackingCode        string // provider tracking code
	InvoiceID           string // provider's id for the linked NFe (optional)
	Status              TrackingStatus
	StatusRawCode       int
	StatusRawName       string
	CreatedAt           time.Time
	// ProviderMeta is the raw response for debugging / auditing. Persisted as JSONB.
	ProviderMeta map[string]any
}

// AttachInvoiceRequest links an already-emitted NFe/DCe to an existing shipment.
type AttachInvoiceRequest struct {
	ProviderOrderID string
	ExternalOrderID string // some providers identify the order by external id
	InvoiceKey      string // NFe or DCe key (44 chars for NFe)
	InvoiceKind     string // "nfe" | "dce"
}

// UploadInvoiceXMLRequest uploads the full NFe XML file.
type UploadInvoiceXMLRequest struct {
	ProviderOrderID string
	ExternalOrderID string
	XML             []byte
	Filename        string // "nfe-12345.xml"
}

// GenerateLabelsRequest identifies which shipments should have labels generated.
// Providers accept multiple identifier types; the caller fills whichever it has.
type GenerateLabelsRequest struct {
	ProviderOrderIDs []string
	TrackingCodes    []string
	InvoiceKeys      []string
	ExternalOrderIDs []string

	// Format is the preferred label format. Providers may ignore unsupported
	// values. Known: "pdf", "zpl", "base64".
	Format string

	// DocumentType controls how the label interacts with the DANFE — when
	// supported by the provider. Known: "label_integrated_danfe", "label_separate_danfe".
	DocumentType string
}

// GenerateLabelsResult contains the URL of the label batch plus per-shipment
// tickets. Shape is normalized across providers.
type GenerateLabelsResult struct {
	LabelURL string
	Tickets  []LabelTicket
}

// LabelTicket represents the labels for a single shipment.
type LabelTicket struct {
	ProviderOrderID string
	TrackingCode    string
	PublicTracking  string   // public URL the customer can check
	VolumeBarcodes  []string // one barcode per physical package
}

// TrackShipmentRequest identifies which shipment to pull tracking for.
// Exactly ONE field should be set.
type TrackShipmentRequest struct {
	ProviderOrderID string
	ExternalOrderID string
	InvoiceKey      string
	TrackingCode    string
}

// TrackShipmentResult contains the normalized tracking history.
type TrackShipmentResult struct {
	TrackingCode  string
	Carrier       string
	Service       string
	CurrentStatus TrackingStatus
	Events        []TrackingEvent
	ProviderMeta  map[string]any
}

// TrackingEvent is a single movement in the tracking history.
type TrackingEvent struct {
	Status      TrackingStatus
	RawCode     int
	RawName     string
	Observation string
	EventAt     time.Time
}

// TrackingStatus is the LiveCart-normalized shipment status. Every provider
// translates its own status codes into this enum so downstream consumers
// (admin UI, notifications, reports) are provider-agnostic.
type TrackingStatus string

const (
	TrackingStatusUnknown                  TrackingStatus = "unknown"
	TrackingStatusAwaitingInvoice          TrackingStatus = "awaiting_invoice"
	TrackingStatusPending                  TrackingStatus = "pending"
	TrackingStatusPendingPickup            TrackingStatus = "pending_pickup"
	TrackingStatusPendingDropoff           TrackingStatus = "pending_dropoff"
	TrackingStatusAwaitingPickup           TrackingStatus = "awaiting_pickup"
	TrackingStatusInTransit                TrackingStatus = "in_transit"
	TrackingStatusOutForDelivery           TrackingStatus = "out_for_delivery"
	TrackingStatusDelivered                TrackingStatus = "delivered"
	TrackingStatusDeliveryIssue            TrackingStatus = "delivery_issue"
	TrackingStatusDeliveryBlocked          TrackingStatus = "delivery_blocked"
	TrackingStatusIssue                    TrackingStatus = "issue"
	TrackingStatusShipmentBlocked          TrackingStatus = "shipment_blocked"
	TrackingStatusDamaged                  TrackingStatus = "damaged"
	TrackingStatusStolen                   TrackingStatus = "stolen"
	TrackingStatusLost                     TrackingStatus = "lost"
	TrackingStatusFiscalIssue              TrackingStatus = "fiscal_issue"
	TrackingStatusRefused                  TrackingStatus = "refused"
	TrackingStatusNotDelivered             TrackingStatus = "not_delivered"
	TrackingStatusIndemnificationRequested TrackingStatus = "indemnification_requested"
	TrackingStatusIndemnificationScheduled TrackingStatus = "indemnification_scheduled"
	TrackingStatusIndemnificationCompleted TrackingStatus = "indemnification_completed"
	TrackingStatusReturning                TrackingStatus = "returning"
	TrackingStatusReturned                 TrackingStatus = "returned"
	TrackingStatusCanceled                 TrackingStatus = "canceled"
)

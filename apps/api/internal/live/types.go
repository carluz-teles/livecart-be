package live

import (
	"time"

	"livecart/apps/api/lib/query"
)

// =============================================================================
// LIVE EVENT - The container for sessions. Carts are tied to events.
// =============================================================================

// Handler layer - Request/Response types for Events
type CreateEventRequest struct {
	Title string `json:"title" validate:"required,min=1,max=200"`
	Type  string `json:"type" validate:"required,oneof=single multi"`
	// Scheduling
	ScheduledAt *string `json:"scheduledAt"` // ISO8601 timestamp
	Description *string `json:"description" validate:"omitempty,max=2000"`
}

type CreateEventResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type EventResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// `type` SAIU do contrato (000120). A espécie da campanha está em
	// `sessionTypes`, mais abaixo: uma campanha mista não tem tipo único, e
	// devolver um rótulo derivado seria a mesma mentira em outro lugar.
	Status                 string `json:"status"`
	TotalOrders            int    `json:"totalOrders"`
	CloseCartOnEventEnd    bool   `json:"closeCartOnEventEnd"`
	CartExpirationMinutes  *int   `json:"cartExpirationMinutes"`
	CartMaxQuantityPerItem *int   `json:"cartMaxQuantityPerItem"`
	SendOnLiveEnd          *bool  `json:"sendOnLiveEnd"`
	PixDiscountPercent     int    `json:"pixDiscountPercent"`
	// RN-10 — janela extra do promovido da fila (minutos).
	WaitlistNotifiedTtlMinutes int `json:"waitlistNotifiedTtlMinutes"`
	// Scheduling
	ScheduledAt *time.Time `json:"scheduledAt"`
	EndsAt      *time.Time `json:"endsAt"`
	Description *string    `json:"description"`
	// Counts
	ProductCount int               `json:"productCount"`
	UpsellCount  int               `json:"upsellCount"`
	Sessions     []SessionResponse `json:"sessions,omitempty"`
	// SessionTypes são os tipos DISTINTOS das transmissões deste evento
	// ({live, post, reel, story}). É a única fonte de "que espécie de evento é
	// este" que sobrevive à 000120, que dropou live_events.type: com a campanha
	// mista o tipo do container deixou de ter resposta única, e quem quiser
	// rotular a tela tem que olhar as sessões. Nunca nulo — evento sem sessão
	// devolve lista vazia, e lista vazia é "ainda não sabemos", não "live".
	SessionTypes []string  `json:"sessionTypes"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type ListEventsResponse struct {
	Data       []EventResponse          `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// EndEventRequest represents the request body for ending an event.
type EndEventRequest struct {
	SendOnLiveEnd *bool `json:"sendOnLiveEnd"` // Optional override
}

// EndEventResponse represents the response after ending an event.
type EndEventResponse struct {
	Event          EventResponse `json:"event"`
	CartsFinalized int           `json:"cartsFinalized"`
	AutoSendLinks  bool          `json:"autoSendLinks"`
}

// Service layer - Event
type CreateEventInput struct {
	StoreID                string
	Title                  string
	Type                   string // single or multi
	CloseCartOnEventEnd    *bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     *int
	// Scheduling
	ScheduledAt *time.Time
	Description *string
}

type CreateEventOutput struct {
	ID        string
	Title     string
	Status    string
	CreatedAt time.Time
}

type EndEventInput struct {
	ID       string
	StoreID  string
	AutoSend *bool // Override store's auto_send_checkout_links setting (nil = use store default)
}

type EndEventOutput struct {
	Event          EventOutput
	CartsFinalized int  // Number of carts moved to checkout
	AutoSendLinks  bool // Whether checkout links will be sent automatically
}

type EventOutput struct {
	ID                     string
	StoreID                string
	Title                  string
	Status                 string
	TotalOrders            int
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	// RN-10 — janela extra do promovido da fila, em minutos.
	WaitlistNotifiedTTLMinutes int
	// Scheduling
	ScheduledAt *time.Time
	EndsAt      *time.Time
	Description *string
	// Counts
	ProductCount int
	UpsellCount  int
	Sessions     []SessionOutput
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ListEventsInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    EventFilters
}

type ListEventsOutput struct {
	Events     []EventOutput
	Total      int
	Pagination query.Pagination
}

type EventFilters struct {
	Status   []string `query:"status"` // scheduled, active, ended
	DateFrom *string  `query:"dateFrom"`
	DateTo   *string  `query:"dateTo"`
}

// Repository layer - Event
type CreateEventParams struct {
	StoreID string
	Title   string
	// Type é o tipo da SESSÃO inicial (D3). O evento não guarda tipo desde a
	// 000120 — aceita o vocabulário legado (single|multi) e traduz.
	Type                   string
	Status                 string
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	// Janela comercial. StartsAt já chega RESOLVIDO (o Service cai para
	// scheduledAt quando o formulário só manda o campo legado) — por isso não
	// há ScheduledAt aqui: duas entradas para as duas colunas que sempre
	// carregam o mesmo valor é como elas divergiriam.
	//
	// EndsAt é OBRIGATÓRIO: a coluna é NOT NULL desde a 000120 e é o teto que
	// garante que nenhum carrinho fica sem prazo.
	StartsAt    *time.Time
	EndsAt      *time.Time
	Description *string
}

type EventRow struct {
	ID                     string
	StoreID                string
	Title                  string
	Status                 string
	TotalOrders            int
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	// RN-10: janela extra, em minutos, que o promovido da fila ganha a partir
	// do momento em que o item libera. Vive em live_events desde a 000073
	// (CHECK 5..240) e o runtime já a aplica — faltava só aparecer na API.
	WaitlistNotifiedTTLMinutes int
	// Scheduling: ScheduledAt is the start; EndsAt is the optional end.
	ScheduledAt *time.Time
	EndsAt      *time.Time
	Description *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// =============================================================================
// LIVE SESSION - Platform-agnostic broadcast with start/end times
// =============================================================================

// Handler layer - Request/Response types for Sessions
type CreateSessionRequest struct {
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
	// Type é a natureza da transmissão (D3). Vazio = "live". Os valores aqui
	// espelham o CHECK live_sessions_type_check da 000109: desalinhar os dois
	// devolve 500 em vez de 422 (lição E6 da errata).
	Type string `json:"type" validate:"omitempty,oneof=live post reel story"`
}

// EventPulse is a tiny change-signal for near-real-time dashboard refresh. The
// client polls it cheaply and only refetches the heavy lists when a field moves:
// Orders/OrdersChangedAt → carts; Comments → comments feed.
type EventPulse struct {
	Orders          int       `json:"orders"`          // new-cart counter (event.total_orders)
	Comments        int       `json:"comments"`        // total comments across sessions
	OrdersChangedAt time.Time `json:"ordersChangedAt"` // latest cart change (catches edits/payments)
}

// CommentResponse represents a single comment in the API response.
type CommentResponse struct {
	Handle string `json:"handle"`
	Text   string `json:"text"`
}

// =============================================================================
// MÉTRICA EM DOIS NÍVEIS (Fatia 5)
//
// Dois níveis, dois grupos de números, nunca misturados:
//   • CONFIRMADO — o congelado do pedido pago (order_items.session_id). É o que
//     entra em relatório de faturamento.
//   • PROJETADO  — o que está nos carrinhos abertos, repartido pelo log de
//     adições. É expectativa, não receita.
//
// Ambos em GMV BRUTO. Desconto de cupom é do EVENTO e frete é do CARRINHO —
// nenhum dos dois tem sessão, então "receita líquida por transmissão" é
// insolúvel por construção e deliberadamente não existe aqui.
// =============================================================================

// SessionRevenueResponse são os números de UMA transmissão, nos dois níveis.
type SessionRevenueResponse struct {
	// Confirmado
	PaidCarts        int   `json:"paidCarts"`
	SoldUnits        int   `json:"soldUnits"`
	ConfirmedRevenue int64 `json:"confirmedRevenue"`
	// Projetado
	OpenCarts        int   `json:"openCarts"`
	ProjectedUnits   int   `json:"projectedUnits"`
	ProjectedRevenue int64 `json:"projectedRevenue"`
}

// SessionMetricsResponse é uma linha do relatório por transmissão. SessionID
// nulo é o balde "sem transmissão" — adição feita pelo painel, ou carrinho
// montado antes do log existir. Ele aparece de propósito: escondê-lo faria a
// soma das linhas não fechar com o total do evento.
type SessionMetricsResponse struct {
	SessionID     *string `json:"sessionId"`
	SequenceOrder int     `json:"sequenceOrder"`
	Type          string  `json:"type"`
	Status        string  `json:"status"`
	// AttributionSource é 'first_touch' quando a transmissão já existia antes
	// do corte da 000119 — os números dela incluem período em que a atribuição
	// creditava o produto inteiro à sessão da PRIMEIRA adição. 'addition_log' é
	// a transmissão nascida depois, 100% derivada do log. A tela precisa disso
	// para avisar; sem o aviso, quem comparar os dois lados conclui que a
	// métrica quebrou.
	AttributionSource string `json:"attributionSource"`
	SessionRevenueResponse
}

// EventSessionMetricsResponse é a métrica do evento com a quebra por
// transmissão. ConfirmedRevenue/ProjectedRevenue são, por construção, a soma
// exata de sessions + unattributed — e batem com o event-stats do evento.
type EventSessionMetricsResponse struct {
	EventID          string                   `json:"eventId"`
	ConfirmedRevenue int64                    `json:"confirmedRevenue"`
	ProjectedRevenue int64                    `json:"projectedRevenue"`
	Sessions         []SessionMetricsResponse `json:"sessions"`
	Unattributed     *SessionMetricsResponse  `json:"unattributed"`
	// AttributionCutoverAt é o instante registrado em metric_cutovers (D26).
	// Nulo só se o marcador não existir no banco — a métrica responde do mesmo
	// jeito, sem a ressalva.
	AttributionCutoverAt *time.Time `json:"attributionCutoverAt"`
	AttributionCutoverNote string   `json:"attributionCutoverNote,omitempty"`
}

type SessionResponse struct {
	ID            string     `json:"id"`
	EventID       string     `json:"eventId"`
	Type          string     `json:"type"`
	Status        string     `json:"status"`
	SequenceOrder int        `json:"sequenceOrder"`
	StartedAt     *time.Time `json:"startedAt"`
	EndedAt       *time.Time `json:"endedAt"`
	TotalComments int        `json:"totalComments"`
	// Métrica desta transmissão (Fatia 5). Os nomes antigos (totalCarts,
	// totalRevenue, paidRevenue) saíram junto com GetSessionStats: eles falavam
	// do carrinho INTEIRO creditado à sessão em que ele nasceu.
	SessionRevenueResponse
	Platforms []PlatformResponse `json:"platforms,omitempty"`
	Comments  []CommentResponse  `json:"comments,omitempty"`
	CreatedAt time.Time          `json:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

// Service layer - Session
type CreateSessionInput struct {
	EventID        string
	StoreID        string
	Type           string
	Platform       string
	PlatformLiveID string
}

type CreateSessionOutput struct {
	ID        string
	EventID   string
	Type      string
	Status    string
	Platform  PlatformOutput
	CreatedAt time.Time
}

// CommentOutput represents a comment in the service layer.
type CommentOutput struct {
	Handle string
	Text   string
}

// SessionRevenueOutput são os números de uma transmissão nos dois níveis
// (Fatia 5). Zero em tudo é resposta legítima: sessão que ainda não vendeu.
type SessionRevenueOutput struct {
	PaidCarts        int
	SoldUnits        int
	ConfirmedRevenue int64
	OpenCarts        int
	ProjectedUnits   int
	ProjectedRevenue int64
}

// SessionMetricsOutput é uma linha do relatório por transmissão. SessionID
// vazio é o balde "sem transmissão".
type SessionMetricsOutput struct {
	SessionID         string
	SequenceOrder     int
	Type              string
	Status            string
	AttributionSource string
	SessionRevenueOutput
}

// EventSessionMetricsOutput é a métrica em dois níveis de um evento.
type EventSessionMetricsOutput struct {
	EventID          string
	ConfirmedRevenue int64
	ProjectedRevenue int64
	Sessions         []SessionMetricsOutput
	// Unattributed é nil quando não há nada sem transmissão.
	Unattributed *SessionMetricsOutput
	// Marcador de corte da atribuição (D26). Nulo quando metric_cutovers não
	// tem a chave — a métrica responde igual, só sem a ressalva.
	AttributionCutoverAt   *time.Time
	AttributionCutoverNote string
}

type SessionOutput struct {
	ID            string
	EventID       string
	Type          string
	Status        string
	SequenceOrder int
	// Modo Live (D17): estado EFÊMERO de execução DESTA transmissão.
	CurrentActiveProductID *string
	ProcessingPaused       bool
	StartedAt              *time.Time
	EndedAt                *time.Time
	TotalComments          int
	SessionRevenueOutput
	Platforms []PlatformOutput
	Comments  []CommentOutput
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Repository layer - Session
type CreateSessionParams struct {
	EventID string
	Status  string
	Type    string
}

type SessionRow struct {
	ID      string
	EventID string
	Type    string
	Status  string
	// A ordem da transmissão dentro da campanha (1ª, 2ª, …). É por ela que a
	// métrica em dois níveis lista as sessões — ListSessionsByEvent devolve por
	// created_at DESC, que é a ordem da tela, não a do relatório.
	SequenceOrder int
	// Modo Live (D17): estado EFÊMERO de execução DESTA transmissão.
	CurrentActiveProductID *string
	ProcessingPaused       bool
	// D26 (000119): 'first_touch' quando a transmissão é anterior ao corte da
	// atribuição, 'addition_log' quando nasceu depois dele.
	AttributionSource string
	StartedAt         *time.Time
	EndedAt           *time.Time
	TotalComments     int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// =============================================================================
// PLATFORM - Platform IDs associated with sessions
// =============================================================================

// PlatformResponse represents a platform ID associated with a session.
type PlatformResponse struct {
	ID             string    `json:"id"`
	Platform       string    `json:"platform"`
	PlatformLiveID string    `json:"platformLiveId"`
	AddedAt        time.Time `json:"addedAt"`
}

type ListPlatformsResponse struct {
	Data []PlatformResponse `json:"data"`
}

type AddPlatformRequest struct {
	Platform       string `json:"platform" validate:"required,oneof=instagram tiktok youtube facebook"`
	PlatformLiveID string `json:"platformLiveId" validate:"required"`
}

// Service layer - Platform
type AddPlatformInput struct {
	SessionID      string
	Platform       string
	PlatformLiveID string
}

type PlatformOutput struct {
	ID             string
	SessionID      string
	Platform       string
	PlatformLiveID string
	AddedAt        time.Time
}

// Repository layer - Platform
type PlatformRow struct {
	ID             string
	SessionID      string
	Platform       string
	PlatformLiveID string
	AddedAt        time.Time
}

// Repository layer - Comment
type CommentRow struct {
	ID                string
	SessionID         string
	PlatformCommentID string
	PlatformUserID    string
	PlatformHandle    string
	Text              string
	HasPurchaseIntent bool
	// Hidden mirrors the Instagram hide state (kept in sync by the moderation
	// actions) so the UI's hide button can toggle hide ↔ unhide.
	Hidden    bool
	CreatedAt time.Time
}

// CommentModerationResponse is a comment returned to the moderation UI, including
// the Instagram comment ID needed to reply / hide / delete via the Graph API.
type CommentModerationResponse struct {
	ID                string    `json:"id"`
	PlatformCommentID string    `json:"platformCommentId"`
	Handle            string    `json:"handle"`
	Text              string    `json:"text"`
	HasPurchaseIntent bool      `json:"hasPurchaseIntent"`
	Hidden            bool      `json:"hidden"`
	CreatedAt         time.Time `json:"createdAt"`
}

// ListCommentsResponse wraps the moderation comment list.
type ListCommentsResponse struct {
	Data []CommentModerationResponse `json:"data"`
}

// PostMediaInput carries the Instagram post metadata when creating a post event.
type PostMediaInput struct {
	MediaID      string
	Permalink    string
	ThumbnailURL string
	Caption      string
}

// MediaRef is a lightweight reference to ONE published media for the capture
// loops. D3/A4: a unidade do polling passou a ser a mídia, não o evento — um
// evento guarda-chuva tem N mídias e cada uma tem seu próprio webhook_active.
type MediaRef struct {
	MediaID       string
	SessionID     string
	SessionType   string
	EventID       string
	StoreID       string
	EventStatus   string
	WebhookActive bool
}

// CreatePostRequest is the HTTP payload to create a post-commerce event.
// StartsAt/EndsAt are optional ISO8601 timestamps (with timezone).
type CreatePostRequest struct {
	Title             string   `json:"title"`
	MediaID           string   `json:"mediaId" validate:"required"`
	MediaPermalink    string   `json:"mediaPermalink"`
	MediaThumbnailURL string   `json:"mediaThumbnailUrl"`
	MediaCaption      string   `json:"mediaCaption"`
	ProductIDs        []string `json:"productIds" validate:"required,min=1"`
	StartsAt          *string  `json:"startsAt"`
	// EndsAt é OBRIGATÓRIO (RN-05/CA-05.1). Sem teto, a RN-04 (expires_at NULL
	// durante o evento) deixa o carrinho sem prazo para sempre.
	EndsAt *string `json:"endsAt" validate:"required"`
	// min=15 espelha o CHECK da migration 000104; abaixo disso o INSERT vira
	// 500 em vez de erro de campo (lição E6 da errata).
	CartExpirationMinutes  *int `json:"cartExpirationMinutes" validate:"omitempty,min=15,max=1440"`
	CartMaxQuantityPerItem *int `json:"cartMaxQuantityPerItem" validate:"omitempty,min=1,max=100"`
}

// CreatePostInput is the input to create a post-commerce event.
type CreatePostInput struct {
	StoreID string
	// Type é o tipo da SESSÃO (D3): "post" (padrão) para post de feed, "reel"
	// para Reels e "story" para Stories (janela de 24h, intenção capturada por
	// resposta de DM). O evento continua rotulado no vocabulário legado.
	Type                   string
	Title                  string
	MediaID                string
	MediaPermalink         string
	MediaThumbnailURL      string
	MediaCaption           string
	ProductIDs             []string
	StartsAt               *time.Time
	EndsAt                 *time.Time
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
}

// =============================================================================
// LEGACY TYPES - For backwards compatibility with existing /lives endpoint
// =============================================================================

// LiveFilters for legacy compatibility
type LiveFilters struct {
	Status   []string `query:"status"`   // scheduled, live, ended, cancelled
	Platform []string `query:"platform"` // instagram, tiktok, youtube, facebook
	DateFrom *string  `query:"dateFrom"`
	DateTo   *string  `query:"dateTo"`
}

// CreateLiveRequest - Creates an event with a session and platform
type CreateLiveRequest struct {
	Title          string  `json:"title" validate:"required,min=1,max=200"`
	Type           string  `json:"type" validate:"omitempty,oneof=single multi"`
	Platform       *string `json:"platform" validate:"omitempty,oneof=instagram"`
	PlatformLiveID *string `json:"platformLiveId" validate:"omitempty"`
	// Scheduling
	ScheduledAt *string `json:"scheduledAt"` // ISO8601 timestamp — legado, sinônimo de startsAt
	// StartsAt/EndsAt são a JANELA COMERCIAL do evento (D21). EndsAt é
	// OBRIGATÓRIO (RN-05/CA-05.1): é o teto que garante que nenhum carrinho
	// fica órfão, já que a RN-04 mantém expires_at NULL durante o evento.
	StartsAt    *string `json:"startsAt"`
	EndsAt      *string `json:"endsAt" validate:"required"`
	Description *string `json:"description" validate:"omitempty,max=2000"`
	// Cart settings (override store defaults)
	CloseCartOnEventEnd *bool `json:"closeCartOnEventEnd"`
	// min=15 espelha o CHECK da migration 000104. Estava em 5 e um valor entre
	// 5 e 14 passava na validação e estourava no banco como 500 (lição E6).
	CartExpirationMinutes  *int  `json:"cartExpirationMinutes" validate:"omitempty,min=15,max=1440"`
	CartMaxQuantityPerItem *int  `json:"cartMaxQuantityPerItem" validate:"omitempty,min=1,max=100"`
	SendOnLiveEnd          *bool `json:"sendOnLiveEnd"`
	// PixDiscountPercent (0-100). 0 disables the feature.
	PixDiscountPercent *int `json:"pixDiscountPercent" validate:"omitempty,min=0,max=100"`
	// RN-10 — janela extra do promovido da fila. O range espelha o CHECK da
	// migration 000073 (5..240): desalinhar devolveria 500 em vez de 422.
	WaitlistNotifiedTtlMinutes *int `json:"waitlistNotifiedTtlMinutes" validate:"omitempty,min=5,max=240"`
}

type CreateLiveResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Platform  string    `json:"platform,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateLiveRequest struct {
	Title string `json:"title" validate:"required,min=1,max=200"`
	// Optional fields. When omitted, the existing value is preserved.
	PixDiscountPercent *int `json:"pixDiscountPercent" validate:"omitempty,min=0,max=100"`
	// Janela comercial (RN-05/CA-05.7). Ponteiro + omissão = "não mexer"; string
	// vazia = "limpar". Sem essa distinção, um PUT que só ajusta o fim apagaria
	// o início. Editar endsAt re-agenda o fechamento — inclusive para MENOS
	// (CA-05.4), que hoje é no-op por causa do asynq.TaskID.
	StartsAt *string `json:"startsAt"`
	EndsAt   *string `json:"endsAt"`
	// RN-10 — mesmo range do CHECK da 000073.
	WaitlistNotifiedTtlMinutes *int `json:"waitlistNotifiedTtlMinutes" validate:"omitempty,min=5,max=240"`
}

type LiveResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// `type` SAIU (000120) — ver `sessionTypes`.
	Platform               string     `json:"platform"`       // Primary platform (from first session)
	PlatformLiveID         string     `json:"platformLiveId"` // Primary platform live ID
	Status                 string     `json:"status"`
	StartedAt              *time.Time `json:"startedAt"`
	EndedAt                *time.Time `json:"endedAt"`
	TotalComments          int        `json:"totalComments"`
	TotalOrders            int        `json:"totalOrders"`
	CloseCartOnEventEnd    bool       `json:"closeCartOnEventEnd"`
	CartExpirationMinutes  *int       `json:"cartExpirationMinutes"`
	CartMaxQuantityPerItem *int       `json:"cartMaxQuantityPerItem"`
	SendOnLiveEnd          *bool      `json:"sendOnLiveEnd"`
	PixDiscountPercent     int        `json:"pixDiscountPercent"`
	// RN-10 — janela extra do promovido da fila (minutos). Existe no banco
	// desde a 000073 e o runtime já a aplica; até aqui não aparecia em DTO
	// nenhum, então o lojista não tinha como ver nem mudar.
	WaitlistNotifiedTtlMinutes int `json:"waitlistNotifiedTtlMinutes"`
	// Scheduling
	ScheduledAt *time.Time `json:"scheduledAt"`
	EndsAt      *time.Time `json:"endsAt"`
	Description *string    `json:"description"`
	// Counts
	ProductCount int `json:"productCount"`
	UpsellCount  int `json:"upsellCount"`
	// SessionTypes — mesma semântica de EventResponse.SessionTypes. A LISTA
	// precisa dele tanto quanto o detalhe: ela não carrega sessions[], então
	// sem este campo a tela de eventos só teria live_events.type para escolher
	// o rótulo — exatamente a coluna que a 000120 removeu.
	SessionTypes []string  `json:"sessionTypes"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type ListLivesResponse struct {
	Data       []LiveResponse           `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// LiveStatsResponse são os contadores do topo de /events.
//
// RN-19: totalLives/activeLives sempre contaram EVENTOS, não lives — o rótulo
// mentia desde antes do guarda-chuva, e agora que uma campanha tem live, post,
// reel e story ao mesmo tempo ele mente de forma visível. Os nomes novos entram
// ao lado dos antigos, com o MESMO valor, para que o frontend possa migrar sem
// deploy acoplado; os antigos saem quando ninguém mais os ler.
type LiveStatsResponse struct {
	TotalEvents  int   `json:"totalEvents"`
	ActiveEvents int   `json:"activeEvents"`
	TotalLives   int   `json:"totalLives"`  // Deprecated: use totalEvents.
	ActiveLives  int   `json:"activeLives"` // Deprecated: use activeEvents.
	TotalOrders  int   `json:"totalOrders"`
	TotalRevenue int64 `json:"totalRevenue"`
}

type EndLiveRequest struct {
	SendOnLiveEnd *bool `json:"sendOnLiveEnd"`
}

type EndLiveResponse struct {
	Live           LiveResponse `json:"live"`
	CartsFinalized int          `json:"cartsFinalized"`
	AutoSendLinks  bool         `json:"autoSendLinks"`
}

// Service layer - Legacy
type CreateLiveInput struct {
	StoreID                string
	Title                  string
	Type                   string
	Platform               *string
	PlatformLiveID         *string
	CloseCartOnEventEnd    *bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     *int
	// RN-10 — janela extra do promovido da fila. nil = mantém o default da coluna.
	WaitlistNotifiedTTLMinutes *int
	// Scheduling
	ScheduledAt *time.Time
	// StartsAt/EndsAt: janela comercial do evento (D21). EndsAt é obrigatório
	// no create — o service recusa quando vem nil.
	StartsAt    *time.Time
	EndsAt      *time.Time
	Description *string
}

type CreateLiveOutput struct {
	ID        string
	Title     string
	Platform  string
	Status    string
	CreatedAt time.Time
}

type UpdateLiveInput struct {
	ID                 string
	StoreID            string
	Title              string
	PixDiscountPercent *int
	// RN-10 — nil = não mexer.
	WaitlistNotifiedTTLMinutes *int
	// Window carrega a alteração PARCIAL da janela comercial.
	Window EventWindowUpdate
}

// EventWindowUpdate descreve uma alteração PARCIAL da janela comercial do
// evento. Os flags Set* existem porque nil tem DOIS sentidos aqui: "não mexer"
// (edição que só ajusta o fim) e "limpar" (evento que deixa de ter início
// agendado). Sem eles, um PUT que só antecipa ends_at apagaria starts_at em
// silêncio — que é exatamente o que o SetEventWindow antigo fazia, por escrever
// as duas colunas de uma vez.
type EventWindowUpdate struct {
	SetStartsAt bool
	StartsAt    *time.Time
	SetEndsAt   bool
	EndsAt      *time.Time
}

// Touches informa se há de fato alguma coluna de janela para escrever.
func (w EventWindowUpdate) Touches() bool { return w.SetStartsAt || w.SetEndsAt }

type EndLiveInput struct {
	ID       string
	StoreID  string
	AutoSend *bool
}

type EndLiveOutput struct {
	Live           LiveOutput
	CartsFinalized int
	AutoSendLinks  bool
}

type ListLivesInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    LiveFilters
}

type ListLivesOutput struct {
	Lives      []LiveOutput
	Total      int
	Pagination query.Pagination
}

type LiveOutput struct {
	ID                     string
	StoreID                string
	Title                  string
	Platform               string // Primary platform
	PlatformLiveID         string // Primary platform live ID
	Status                 string
	StartedAt              *time.Time
	EndedAt                *time.Time
	TotalComments          int
	TotalOrders            int
	// SessionTypes são os tipos DISTINTOS das sessões deste evento. É o
	// substituto de Type, que a 000120 dropou.
	SessionTypes           []string
	CloseCartOnEventEnd    bool
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	SendOnLiveEnd          *bool
	PixDiscountPercent     int
	// RN-10 — janela extra do promovido da fila, em minutos (CHECK 5..240).
	WaitlistNotifiedTTLMinutes int
	// Scheduling: ScheduledAt is the start, EndsAt the optional scheduled end.
	ScheduledAt *time.Time
	EndsAt      *time.Time
	Description *string
	// Counts
	ProductCount int
	UpsellCount  int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type LiveStatsOutput struct {
	TotalLives   int
	ActiveLives  int
	TotalOrders  int
	TotalRevenue int64
}

// =============================================================================
// CART - Service layer
// =============================================================================

// AddToCartInput represents input for adding a product to a user's cart during a live.
type AddToCartInput struct {
	StoreID            string // Required - used to upsert the customer row
	EventID            string // Changed from SessionID to EventID
	SessionID          string // Optional - session the item is attributed to (first-touch)
	PlatformUserID     string
	PlatformHandle     string
	ProductID          string
	ProductPrice       int64
	Quantity           int     // Total quantity to add
	WaitlistedQuantity int     // How many of the quantity are waitlisted (0 = all available)
	CustomerID         *string // Optional - links cart to a customer (if set, skips upsert)
}

// AddToCartOutput represents the result of adding to cart.
type AddToCartOutput struct {
	CartID     string
	CartToken  string
	IsNewCart  bool
	TotalItems int   // Total items in cart after add
	TotalCents int64 // Total value in cents after add
}

// GetOrCreateCartParams represents parameters for GetOrCreateCart.
type GetOrCreateCartParams struct {
	EventID        string
	SessionID      *string // Optional - tracks which session created the cart
	PlatformUserID string
	PlatformHandle string
	Token          string
	CustomerID     *string // Optional - links cart to a customer
}

// CartRow represents a cart row from the database.
type CartRow struct {
	ID             string
	EventID        string // Changed from SessionID to EventID
	PlatformUserID string
	PlatformHandle string
	Token          string
}

// AddCartItemParams represents parameters for adding an item to a cart.
type AddCartItemParams struct {
	CartID             string
	ProductID          string
	SessionID          string // Optional - session the item is attributed to (first-touch)
	Quantity           int
	UnitPrice          int64
	WaitlistedQuantity int // How many of the quantity are waitlisted
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing for event details page
// =============================================================================

// Handler layer - Event details
type EventStatsResponse struct {
	// Funnel metrics
	TotalComments int `json:"totalComments"`
	TotalCarts    int `json:"totalCarts"`
	OpenCarts     int `json:"openCarts"`
	CheckoutCarts int `json:"checkoutCarts"`
	PaidCarts     int `json:"paidCarts"`
	// Product metrics
	TotalProductsSold int `json:"totalProductsSold"`
	// Revenue metrics
	ProjectedRevenue int64 `json:"projectedRevenue"`
	ConfirmedRevenue int64 `json:"confirmedRevenue"`
}

type CartWithTotalResponse struct {
	ID              string     `json:"id"`
	Token           string     `json:"token"`
	SessionID       *string    `json:"sessionId"`
	PlatformUserID  string     `json:"platformUserId"`
	PlatformHandle  string     `json:"platformHandle"`
	Status          string     `json:"status"`
	PaymentStatus   *string    `json:"paymentStatus"`
	TotalValue      int64      `json:"totalValue"`
	TotalItems      int        `json:"totalItems"`
	AvailableItems  int        `json:"availableItems"`
	WaitlistedItems int        `json:"waitlistedItems"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       *time.Time `json:"expiresAt"`
}

type ListCartsResponse struct {
	Data []CartWithTotalResponse `json:"data"`
}

type EventProductSalesResponse struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	ImageURL      *string `json:"imageUrl"`
	Keyword       string  `json:"keyword"`
	TotalQuantity int     `json:"totalQuantity"`
	TotalRevenue  int64   `json:"totalRevenue"`
}

type ListEventProductSalesResponse struct {
	Data []EventProductSalesResponse `json:"data"`
}

// Service layer - Event details
type EventStatsOutput struct {
	// Funnel metrics
	TotalComments int
	TotalCarts    int
	OpenCarts     int
	CheckoutCarts int
	PaidCarts     int
	// Product metrics
	TotalProductsSold int
	// Revenue metrics
	ProjectedRevenue int64
	ConfirmedRevenue int64
}

type CartWithTotalOutput struct {
	ID              string
	Token           string
	SessionID       *string
	PlatformUserID  string
	PlatformHandle  string
	Status          string
	PaymentStatus   *string
	TotalValue      int64
	TotalItems      int
	AvailableItems  int
	WaitlistedItems int
	CreatedAt       time.Time
	ExpiresAt       *time.Time
}

type EventProductSalesOutput struct {
	ID            string
	Name          string
	ImageURL      *string
	Keyword       string
	TotalQuantity int
	TotalRevenue  int64
}

// Repository layer - Event details
type EventStatsRow struct {
	TotalComments     int
	TotalCarts        int
	OpenCarts         int
	CheckoutCarts     int
	PaidCarts         int
	TotalProductsSold int
	ProjectedRevenue  int64
	ConfirmedRevenue  int64
}

type CartWithTotalRow struct {
	ID              string
	EventID         string
	SessionID       *string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	PaymentStatus   *string
	TotalValue      int64
	TotalItems      int
	AvailableItems  int
	WaitlistedItems int
	CreatedAt       time.Time
	ExpiresAt       *time.Time
}

type EventProductRow struct {
	ID            string
	Name          string
	ImageURL      *string
	Keyword       string
	TotalQuantity int
	TotalRevenue  int64
}

// =============================================================================
// ACTIVE CHECKOUTS — live merchant view of carts in checkout phase
// =============================================================================

type ActiveCheckoutRow struct {
	ID                   string
	PlatformHandle       string
	Token                string
	Status               string
	PaymentStatus        string
	CreatedAt            time.Time
	ExpiresAt            *time.Time
	InitialSubtotalCents int64
	CurrentSubtotalCents int64
	MutationCount        int
	LastMutationAt       *time.Time
}

type ActiveCheckoutOutput = ActiveCheckoutRow

type ActiveCheckoutResponse struct {
	ID                   string     `json:"id"`
	PlatformHandle       string     `json:"platformHandle"`
	Token                string     `json:"token"`
	Status               string     `json:"status"`
	PaymentStatus        string     `json:"paymentStatus,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	InitialSubtotalCents int64      `json:"initialSubtotalCents"`
	CurrentSubtotalCents int64      `json:"currentSubtotalCents"`
	DeltaCents           int64      `json:"deltaCents"`
	MutationCount        int        `json:"mutationCount"`
	LastMutationAt       *time.Time `json:"lastMutationAt,omitempty"`
}

type ListActiveCheckoutsResponse struct {
	Data []ActiveCheckoutResponse `json:"data"`
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// Handler layer - Live Mode
type SetActiveProductRequest struct {
	ProductID *string `json:"productId"` // nil to clear
}

type SetProcessingPausedRequest struct {
	Paused bool `json:"paused"`
}

type LiveModeStateResponse struct {
	SessionID        string                 `json:"sessionId"`
	ProcessingPaused bool                   `json:"processingPaused"`
	ActiveProduct    *ActiveProductResponse `json:"activeProduct"`
}

type ActiveProductResponse struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Keyword  string  `json:"keyword"`
	Price    int64   `json:"price"`
	ImageURL *string `json:"imageUrl"`
}

// Service layer - Live Mode
type LiveModeStateOutput struct {
	// SessionID é a transmissão de onde o estado veio (D17). Na rota legada por
	// evento ele diz QUAL sessão o painel está de fato controlando.
	SessionID        string
	ProcessingPaused bool
	ActiveProduct    *ActiveProductOutput
}

type ActiveProductOutput struct {
	ID       string
	Name     string
	Keyword  string
	Price    int64
	ImageURL *string
}

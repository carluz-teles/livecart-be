package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// EventCloseScheduler arms an ETA task that finalizes a timed post/story event
// at its ends_at, so the window closes precisely instead of waiting on the
// SweepEndedTimedEvents sweep (kept as a safety net). Implemented over the asynq
// client in main.go — the live package must not import events at the service
// level, so this stays a local interface wired via SetEventCloseScheduler.
type EventCloseScheduler interface {
	ScheduleEventClose(ctx context.Context, eventID, storeID string, at time.Time) error
	// RescheduleEventClose MOVE um fechamento já armado. Existe separado porque
	// ScheduleEventClose é deduplicado por TaskID ("event-close:<id>"): enquanto
	// a task pendente existir, o horário novo é ignorado. Era por isso que
	// ANTECIPAR ends_at fechava o evento na hora antiga (CA-05.4) — e, na
	// prática, estender também não funcionava.
	RescheduleEventClose(ctx context.Context, eventID, storeID string, at time.Time) error
}

// Notifier is the minimal notification surface this package depends on.
// The concrete implementation lives in the integration package; we declare
// a local interface to avoid an import cycle.
type Notifier interface {
	NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) (NotifyEventCheckoutResult, error)
}

// NotifyEventCheckoutParams mirrors the integration package params struct
// (Go duck typing only matches methods, so we declare the input shape here).
type NotifyEventCheckoutParams struct {
	StoreID        string
	EventID        string
	EventTitle     string
	CartID         string
	CartToken      string
	PlatformUserID string
	PlatformHandle string
	CommentID      string
	// CommentCreatedAt é quando o comentário escolhido foi feito. É o dado que
	// permite decidir entre TENTAR e registrar não-entrega com motivo (RN-38):
	// o private reply do Instagram vale 7 dias, e numa campanha longa esta
	// mensagem dispara dias depois do último comentário do comprador.
	CommentCreatedAt *time.Time
	// DeadlineAt é o prazo que a mensagem anuncia ({prazo_final}). Vem do
	// expires_at que o fechamento acabou de armar no carrinho — os dois ramos
	// do close_cart_on_event_end (prazo curto e prazo estendido) já chegam aqui
	// com o valor certo, então a mensagem não precisa saber qual foi.
	DeadlineAt *time.Time
	TotalItems int
	TotalValue int64
}

// NotifyEventCheckoutResult diz o que aconteceu com a mensagem. Não entregue
// NÃO é erro: é um fato registrado, com motivo, que o painel mostra ao lojista
// (RN-38). Devolver isso como error faria o disparo em massa tratar "a janela
// do Instagram fechou" como falha de infraestrutura e logar ruído; devolver
// como sucesso mudo seria a ilusão de entrega que a regra proíbe.
type NotifyEventCheckoutResult struct {
	Delivered bool
	// Reason é o motivo canônico (vazio quando entregue).
	Reason string
	// ReasonText é a frase pronta para o lojista.
	ReasonText string
}

// CustomerUpserter creates or updates a customer record from cart-creation
// inputs and returns the customer UUID. Wired from the customer package via
// SetCustomerUpserter to keep this package free of customer internals.
type CustomerUpserter interface {
	UpsertForCart(ctx context.Context, storeID, platformUserID, platformHandle string) (customerID string, err error)
}

type Service struct {
	repo             *Repository
	logger           *zap.Logger
	notifier         Notifier
	customerUpserter CustomerUpserter
	closeScheduler   EventCloseScheduler
	ingestRepo       IngestRepository

	// Comment ingestion core collaborators (Bloco B4b). All five are wired via
	// setters AFTER the services are built (integration.Service is the concrete
	// impl and depends on live.Service), so every use is nil-guarded lazily.
	// stockReserver breaks the erp import cycle (neutral ReserveParams);
	// billingGate/webhookAuditor/socialReplier are satisfied by integration.Service
	// directly; notificationSvc is imported directly (notification has no cycle).
	stockReserver   StockReserver
	billingGate     BillingGate
	webhookAuditor  WebhookAuditor
	socialReplier   SocialReplier
	notificationSvc *notification.Service

	// core is the slice of this Service's own behaviour the comment consumer
	// reuses (session/event lookups, AddToCart, event-product whitelist). It
	// defaults to the Service itself (wired in NewService) so production is a
	// plain self-call; unit tests swap in a fake to exercise the core without a
	// database. See the commentCore interface in comment.go.
	core commentCore
}

// SetEventCloseScheduler wires the ETA scheduler for timed-event window close
// (optional — when unset, only SweepEndedTimedEvents finalizes timed events).
func (s *Service) SetEventCloseScheduler(sch EventCloseScheduler) { s.closeScheduler = sch }

func NewService(repo *Repository, logger *zap.Logger) *Service {
	s := &Service{
		repo:   repo,
		logger: logger.Named("live"),
	}
	// The comment core reuses this Service's own methods through the commentCore
	// seam; in production that is the Service itself.
	s.core = s
	return s
}

// SetStockReserver wires the local+ERP stock reservation collaborator used by the
// comment ingestion core. Optional — nil means stock reservations are skipped.
func (s *Service) SetStockReserver(r StockReserver) { s.stockReserver = r }

// SetBillingGate wires the paywall gate (PRD 007). Optional — nil means no gating.
func (s *Service) SetBillingGate(g BillingGate) { s.billingGate = g }

// SetWebhookAuditor wires the webhook-event audit sink. Optional — nil skips audit.
func (s *Service) SetWebhookAuditor(a WebhookAuditor) { s.webhookAuditor = a }

// SetSocialReplier wires the Instagram DM / comment-reply ACL. Optional — nil
// skips replies (max-quantity and post-commerce answers).
func (s *Service) SetSocialReplier(r SocialReplier) { s.socialReplier = r }

// SetNotificationService wires the notification service used for the immediate
// checkout DM/email. Optional — nil skips immediate notifications.
func (s *Service) SetNotificationService(svc *notification.Service) { s.notificationSvc = svc }

// SetNotifier wires a Notifier into the service after construction. This
// breaks the dependency cycle between live and integration packages
// (integration.Service depends on live.Service, and the notifier impl
// depends on integration.Service).
func (s *Service) SetNotifier(n Notifier) {
	s.notifier = n
}

// SetCustomerUpserter wires a CustomerUpserter into the service. When set,
// AddToCart will materialize a customer row and link the cart to it before
// creating the cart, so the Customers tab and downstream analytics always
// reflect the latest activity.
func (s *Service) SetCustomerUpserter(u CustomerUpserter) {
	s.customerUpserter = u
}

// =============================================================================
// LEGACY API - Creates an event + session + platform in one call
// =============================================================================

// Create creates a live event with an optional initial session and platform.
// Uses transactions to ensure atomicity when session/platform are included.
func (s *Service) Create(ctx context.Context, input CreateLiveInput) (CreateLiveOutput, error) {
	// RN-05: ends_at é o TETO da campanha e não tem default razoável. A RN-04
	// deixa expires_at NULL durante o evento inteiro, então um evento sem fim é
	// um carrinho sem prazo e um estoque reservado para sempre. O banco reforça
	// com NOT NULL desde a 000122; esta checagem existe para a resposta ser 400
	// com texto, e não 500 vindo de uma constraint.
	if input.EndsAt == nil {
		return CreateLiveOutput{}, httpx.ErrBadRequest("endsAt é obrigatório: o evento precisa de uma data de encerramento")
	}
	// starts_at é a coluna nova (D21); scheduled_at continua sendo o que decide
	// o status inicial. Quem só manda scheduledAt (o formulário atual) tem os
	// dois preenchidos com o mesmo valor.
	startsAt := input.StartsAt
	if startsAt == nil {
		startsAt = input.ScheduledAt
	}
	if startsAt != nil && !input.EndsAt.After(*startsAt) {
		return CreateLiveOutput{}, httpx.ErrBadRequest("endsAt precisa ser depois de startsAt")
	}

	// Default close_cart_on_event_end to true if not specified
	closeCartOnEventEnd := true
	if input.CloseCartOnEventEnd != nil {
		closeCartOnEventEnd = *input.CloseCartOnEventEnd
	}

	// Determine initial status based on scheduling
	status := "active"
	if input.ScheduledAt != nil {
		status = "scheduled"
	}

	// A mídia é OPCIONAL na criação; a SESSÃO não é.
	//
	// Havia dois caminhos aqui — com plataforma (evento+sessão numa transação) e
	// sem plataforma (só o evento) — e o segundo produzia um evento SEM SESSÃO
	// NENHUMA. Isso era inofensivo enquanto whitelist e modo live moravam em
	// live_events; desde a 000112/000113 eles moram em live_sessions, e o evento
	// sem sessão deixou de ter onde guardar configuração: POST /lives/:id/whitelist
	// gravava zero linhas e respondia 404 ("produto nao esta na whitelist"), e
	// destacar produto virava no-op silencioso. O formulário de evento do painel
	// só manda platformLiveId quando o lojista já tem o id da live — ou seja, o
	// caminho quebrado era o CAMINHO PADRÃO de criar um evento.
	//
	// A D1 já prevê "criar a sessão antes de a mídia existir": a sessão nasce
	// aqui e a mídia entra depois por AddPlatformToSession. Os dois ramos viram
	// um só — a duplicação era o que deixava o ramo sem plataforma para trás a
	// cada regra nova (a janela e o TTL da fila já tiveram de ser escritos duas
	// vezes logo abaixo).
	platform, platformLiveID := "", ""
	if input.Platform != nil {
		platform = *input.Platform
	}
	if input.PlatformLiveID != nil {
		platformLiveID = *input.PlatformLiveID
	}
	if platform == "" || platformLiveID == "" {
		platform, platformLiveID = "", ""
	}

	// A janela vai DENTRO da transação de criação. Antes ela era um UPDATE
	// posterior (applyEventWindow), e um evento cujo UPDATE falhasse ficava
	// commitado sem teto — exatamente o estado que a RN-05 existe para impedir.
	// Aqui embaixo applyEventWindow continua sendo chamado, mas só para ARMAR o
	// fechamento por ETA; a escrita já aconteceu.
	event, session, _, err := s.repo.CreateEventWithSessionTx(ctx, CreateEventParams{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		Type:                   input.Type,
		Status:                 status,
		CloseCartOnEventEnd:    closeCartOnEventEnd,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
		SendOnLiveEnd:          input.SendOnLiveEnd,
		StartsAt:               startsAt,
		EndsAt:                 input.EndsAt,
		Description:            input.Description,
		ProductIDs:             input.ProductIDs,
	}, platform, platformLiveID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to create live with session",
			zap.Error(err),
		)
		return CreateLiveOutput{}, err
	}

	// Metadados da publicação escolhida como PRIMEIRA transmissão. Sem isto, a
	// MESMA publicação nascia rica pelo caminho de evento-de-post (CreatePostEvent
	// chama SetMedia) e pobre pelo formulário de campanha — sem permalink, sem
	// capa, sem legenda. A captura funcionava nos dois (quem resolve o comentário
	// é o platform_live_id), só a tela ficava mais pobre conforme a porta.
	//
	// Best-effort pelo mesmo motivo dos outros dois pontos: metadado é enfeite,
	// e derrubar a criação da campanha por causa da miniatura seria trocar a
	// venda pela capa.
	if platformLiveID != "" && (input.MediaPermalink != "" || input.MediaThumbnailURL != "" || input.MediaCaption != "") {
		if err := s.repo.SetMedia(ctx, PostMediaInput{
			MediaID:      platformLiveID,
			Permalink:    input.MediaPermalink,
			ThumbnailURL: input.MediaThumbnailURL,
			Caption:      input.MediaCaption,
		}); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set first session media metadata",
				zap.String("event_id", event.ID), zap.Error(err))
		}
	}

	// session_id vazio é o caso normal da campanha sem transmissão — ela nasce
	// vazia e as transmissões entram depois.
	logger.From(ctx, s.logger).Info("live event created",
		zap.String("event_id", event.ID),
		zap.String("session_id", session.ID),
		zap.String("platform", platform),
		zap.String("platform_live_id", platformLiveID),
	)

	// Persist the optional pix discount as a follow-up update — the column
	// is not yet wired into the sqlc-generated INSERT.
	if input.PixDiscountPercent != nil && *input.PixDiscountPercent > 0 {
		if err := s.repo.SetPixDiscountPercent(ctx, event.ID, input.StoreID, *input.PixDiscountPercent); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set pix_discount_percent on create",
				zap.String("event_id", event.ID),
				zap.Error(err),
			)
		}
	}

	if input.WaitlistNotifiedTTLMinutes != nil {
		if err := s.repo.SetWaitlistNotifiedTTLMinutes(ctx, event.ID, input.StoreID, *input.WaitlistNotifiedTTLMinutes); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set waitlist_notified_ttl_minutes on create",
				zap.String("event_id", event.ID),
				zap.Error(err),
			)
		}
	}

	// A janela já foi gravada pelo INSERT; aqui só arma o fechamento por ETA.
	s.armEventClose(ctx, event.ID, input.StoreID, input.EndsAt, false)

	return CreateLiveOutput{
		ID:        event.ID,
		Title:     event.Title,
		Platform:  platform,
		Status:    event.Status,
		CreatedAt: event.CreatedAt,
	}, nil
}

// applyEventWindow é o ponto ÚNICO onde a janela comercial é gravada e o
// fechamento por ETA é (re)armado. As três entradas de janela — criar live,
// criar post/reel/story e editar — passam por aqui para que "gravar ends_at" e
// "mover a task de fechamento" nunca saiam de sincronia. Era esse desencontro
// que fazia o lojista antecipar o fim na tela e o evento fechar na hora antiga.
//
// move=true quando o evento JÁ existia e o horário pode ter mudado: aí é
// preciso APAGAR a task pendente antes de re-enfileirar, senão o asynq engole o
// re-agendamento pelo TaskID repetido (CA-05.3/CA-05.4).
//
// Gravar a janela é obrigatório (erro sobe); armar a task é best-effort — o
// SweepEndedTimedEvents é a rede para uma task perdida.
func (s *Service) applyEventWindow(ctx context.Context, eventID, storeID string, w EventWindowUpdate, move bool) error {
	if err := s.repo.SetEventWindow(ctx, eventID, storeID, w); err != nil {
		return err
	}
	if !w.SetEndsAt {
		return nil
	}
	s.armEventClose(ctx, eventID, storeID, w.EndsAt, move)
	return nil
}

// armEventClose (re)agenda a task ETA de fechamento. Separado de
// applyEventWindow porque a CRIAÇÃO já grava a janela dentro da transação do
// INSERT: reescrever as mesmas colunas logo depois seria um UPDATE que não muda
// nada e uma segunda chance de a janela divergir do que foi commitado.
//
// Best-effort de propósito: o SweepEndedTimedEvents é a rede para uma task
// perdida. Falhar a criação do evento porque o agendador está fora do ar seria
// trocar um atraso de até 5 minutos por uma venda que não acontece.
func (s *Service) armEventClose(ctx context.Context, eventID, storeID string, endsAt *time.Time, move bool) {
	if s.closeScheduler == nil || endsAt == nil {
		// endsAt nil = janela removida. A task antiga continua pendente, mas
		// RunScheduledEventClose é guard-first: com ends_at NULL ele sai sem
		// fechar nada.
		return
	}
	var err error
	if move {
		err = s.closeScheduler.RescheduleEventClose(ctx, eventID, storeID, *endsAt)
	} else {
		err = s.closeScheduler.ScheduleEventClose(ctx, eventID, storeID, *endsAt)
	}
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to arm event window close",
			zap.String("event_id", eventID), zap.Bool("move", move), zap.Error(err))
	}
}

// CreatePostEvent creates a post-commerce event mapped to a published Instagram
// post. It reuses the live create path (the post media id is stored as the
// session platform_live_id so comment processing finds the event), persists the
// post metadata, and restricts the created SESSION to the selected products.
//
// Os produtos escolhidos no formulário são da TRANSMISSÃO que nasce aqui, não da
// campanha: é justamente por isso que o atalho continua exigindo pelo menos um
// produto. Eles são gravados DENTRO da transação de criação (ver
// CreateEventWithSessionTx) — antes eram um laço posterior que só logava Warn a
// cada falha, e a publicação já no ar ficava com lista parcial ou vazia. Vazia,
// sob a regra "vazia vende tudo", libera o catálogo inteiro: o oposto do pedido.
func (s *Service) CreatePostEvent(ctx context.Context, input CreatePostInput) (CreateLiveOutput, error) {
	if input.MediaID == "" {
		return CreateLiveOutput{}, httpx.DomainError(400, httpx.CodeLiveMediaRequired, "mediaId is required")
	}
	if len(input.ProductIDs) == 0 {
		return CreateLiveOutput{}, httpx.DomainError(400, httpx.CodeLiveProductRequired, "select at least one product for the promotion")
	}

	platform := "instagram"
	closeCart := true
	eventType := input.Type
	if eventType == "" {
		eventType = "post"
	}
	// Create the event as 'active' regardless of a future start: the effective
	// status (and comment gating) is derived from the window, so the event is
	// always resolvable by media id. We do NOT pass ScheduledAt to Create here,
	// because that would set status='scheduled' and hide it from lookups.
	// A janela (starts_at/ends_at) e o arm do fechamento agora saem de dentro do
	// Create — é lá que a obrigatoriedade de ends_at (RN-05) é aplicada para
	// TODOS os tipos, não só para live.
	out, err := s.Create(ctx, CreateLiveInput{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		Type:                   eventType,
		Platform:               &platform,
		PlatformLiveID:         &input.MediaID,
		CloseCartOnEventEnd:    &closeCart,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
		StartsAt:               input.StartsAt,
		EndsAt:                 input.EndsAt,
		ProductIDs:             input.ProductIDs,
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	// Metadados da publicação: gravados NA MÍDIA, não no evento.
	if err := s.repo.SetMedia(ctx, PostMediaInput{
		MediaID:      input.MediaID,
		Permalink:    input.MediaPermalink,
		ThumbnailURL: input.MediaThumbnailURL,
		Caption:      input.MediaCaption,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("failed to set post media metadata",
			zap.String("event_id", out.ID), zap.Error(err))
	}

	logger.From(ctx, s.logger).Info("post event created",
		zap.String("event_id", out.ID),
		zap.String("media_id", input.MediaID),
		zap.Int("product_count", len(input.ProductIDs)),
	)

	out.Platform = platform
	return out, nil
}

// MarkMediaWebhookActive flags that a comments webhook arrived for THIS media,
// so the polling capture stops handling it. Escopo de mídia, não de evento: um
// evento guarda-chuva tem N mídias e cada uma migra do polling para o webhook
// no seu próprio tempo.
func (s *Service) MarkMediaWebhookActive(ctx context.Context, mediaID string) error {
	return s.repo.MarkMediaWebhookActive(ctx, mediaID)
}

// ListPollableMedia returns the media still served by the polling capture.
func (s *Service) ListPollableMedia(ctx context.Context) ([]MediaRef, error) {
	return s.repo.ListPollableMedia(ctx)
}

// EndEventByMediaID closes the post/reel/story event that owns mediaID when its
// media is gone on Instagram. D5: routes through End so the carts are finalized
// (moved to 'checkout' with expires_at armed → eligible for the expiry worker)
// and the ERP is reconciled — not just a bare status flip that left carts
// 'active' without a deadline forever. Falls back to the raw flip if the event
// can't be resolved.
func (s *Service) EndEventByMediaID(ctx context.Context, mediaID string) error {
	ref, err := s.repo.GetActiveTimedEventByMediaID(ctx, mediaID)
	if err != nil {
		return err
	}
	if ref == nil {
		// Nothing active to finalize; ensure polling stops anyway.
		return s.repo.EndEventByMediaID(ctx, mediaID)
	}
	// post.window_closed: the media was deleted / window closed (distinct from a
	// manual live end). Emitted before End (which fires event.ended).
	if err := s.repo.EmitPostWindowClosed(ctx, ref.EventID); err != nil {
		logger.From(ctx, s.logger).Error("failed to emit post.window_closed",
			zap.String("event_id", ref.EventID), zap.Error(err))
	}
	if _, err := s.End(ctx, EndLiveInput{ID: ref.EventID, StoreID: ref.StoreID}); err != nil {
		return err
	}
	return nil
}

// SweepScheduledEventsReadyToStart ativa os eventos agendados cuja hora marcada
// já chegou. É a outra metade da E37: o botão "Iniciar live" resolve o evento de
// live que o lojista acompanha, mas post/reel/story publicados por agendamento
// não têm ninguém para apertar botão nenhum — sem este sweep eles nasceriam
// 'scheduled' e morreriam 'ended' sem nunca aceitar um comentário.
//
// ORDEM IMPORTA e é decidida aqui, não pelo acaso do agendamento: este sweep
// roda ANTES de SweepEndedTimedEvents no mesmo tick. Os dois predicados são
// disjuntos de propósito (ListEventsReadyToStart exige ends_at ainda no futuro),
// então um evento nunca é ativado e encerrado no mesmo ciclo — quem passou do
// fim vai direto para 'ended', sem o event.ended extra de um active fantasma.
//
// É a REDE do horário, com granularidade do ticker (5 min). Quem quiser abrir na
// hora exata usa o botão. Não há task ETA de abertura porque a janela não é
// consumida só pelo status: WindowAt já usa starts_at/ends_at e um atraso de
// minutos na ativação não vende nada fora do prazo — só adia a abertura.
func (s *Service) SweepScheduledEventsReadyToStart(ctx context.Context) {
	events, err := s.repo.ListEventsReadyToStart(ctx, 200)
	if err != nil {
		logger.From(ctx, s.logger).Error("activation sweep: failed to list scheduled events", zap.Error(err))
		return
	}
	for _, ev := range events {
		evCtx := logger.WithStore(ctx, ev.StoreID, "")
		activated, err := s.repo.ActivateScheduledEvent(evCtx, ev.EventID)
		if err != nil {
			logger.From(evCtx, s.logger).Error("activation sweep: failed to activate event",
				zap.String("event_id", ev.EventID), zap.Error(err))
			continue
		}
		if activated {
			logger.From(evCtx, s.logger).Info("scheduled event activated by sweep",
				zap.String("event_id", ev.EventID))
		}
	}
}

// SweepEndedTimedEvents finaliza TODO evento cuja janela (ends_at) fechou —
// não mais só post/reel/story. D5/RN-05: ends_at deixou de ser "horário
// nominal" e virou o teto contratual da campanha, então restringir o sweep por
// tipo deixava o evento de live com ends_at vencido aberto para sempre e seus
// carrinhos sem prazo (a RN-04 mantém expires_at NULL enquanto o evento está
// aberto). Sem isto, o evento só mudava de status *efetivo* na leitura.
//
// É a REDE, não o caminho principal: o preciso é a task ETA event.window_close.
// End() é idempotente, então varrer um evento já encerrado é no-op.
func (s *Service) SweepEndedTimedEvents(ctx context.Context) {
	events, err := s.repo.ListEventsPastEndsAt(ctx, 200)
	if err != nil {
		logger.From(ctx, s.logger).Error("ends_at sweep: failed to list events", zap.Error(err))
		return
	}
	for _, ev := range events {
		evCtx := logger.WithStore(ctx, ev.StoreID, "")
		// post.window_closed before End (which fires event.ended). The list query
		// only returns still-active events, so this fires once per window close.
		if err := s.repo.EmitPostWindowClosed(evCtx, ev.EventID); err != nil {
			logger.From(evCtx, s.logger).Error("ends_at sweep: failed to emit post.window_closed",
				zap.String("event_id", ev.EventID), zap.Error(err))
		}
		if _, err := s.End(evCtx, EndLiveInput{ID: ev.EventID, StoreID: ev.StoreID}); err != nil {
			logger.From(evCtx, s.logger).Error("ends_at sweep: failed to finalize event",
				zap.String("event_id", ev.EventID),
				zap.Error(err))
		}
	}
}

// RunScheduledEventClose is the event.window_close ETA-task handler: the precise
// per-event counterpart of SweepEndedTimedEvents. Guard-first — if the window
// was pushed out it re-arms, if the event is already ended it is a no-op — then
// runs the same finalization (post.window_closed + End) as the sweep. End is
// idempotent, so at-least-once redelivery is safe.
func (s *Service) RunScheduledEventClose(ctx context.Context, eventID, storeID string) error {
	ev, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil || ev == nil {
		// Not found / unreadable — the sweep is the backstop; don't retry forever.
		return nil
	}
	if ev.EndsAt == nil || ev.Status == "ended" {
		return nil // window removed, or already finalized
	}
	if ev.EndsAt.After(time.Now().UTC()) {
		// A janela mudou depois que a task foi armada e o caminho de edição não
		// re-agendou (edição direta no banco, task duplicada). Tentativa
		// best-effort: como ESTA task está ACTIVE agora, o asynq recusa apagá-la
		// e o TaskID em conflito engole o novo horário. Quem garante o
		// fechamento nesse caso é o sweep de ends_at.
		if s.closeScheduler != nil {
			return s.closeScheduler.ScheduleEventClose(ctx, eventID, storeID, *ev.EndsAt)
		}
		return nil
	}
	// post.window_closed before End (which fires event.ended), mirroring the sweep.
	if err := s.repo.EmitPostWindowClosed(ctx, eventID); err != nil {
		logger.From(ctx, s.logger).Error("scheduled close: failed to emit post.window_closed",
			zap.String("event_id", eventID), zap.Error(err))
	}
	if _, err := s.End(ctx, EndLiveInput{ID: eventID, StoreID: storeID}); err != nil {
		return fmt.Errorf("scheduled close: finalizing event %s: %w", eventID, err)
	}
	return nil
}

// GetEventPulse returns the cheap change-signal used for near-real-time refresh.
func (s *Service) GetEventPulse(ctx context.Context, eventID, storeID string) (EventPulse, error) {
	return s.repo.GetEventPulse(ctx, eventID, storeID)
}

func (s *Service) GetByID(ctx context.Context, id, storeID string) (LiveOutput, error) {
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Hydrate the pix-discount column (not yet wired through sqlc).
	if pct, err := s.repo.GetPixDiscountPercent(ctx, event.ID); err == nil {
		event.PixDiscountPercent = pct
	}

	// Get sessions for this event
	sessions, err := s.repo.ListSessionsByEvent(ctx, event.ID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get platform info from first session
	var platform, platformLiveID string
	var startedAt, endedAt = event.CreatedAt, event.UpdatedAt
	var totalComments int

	if len(sessions) > 0 {
		firstSession := sessions[0]
		if firstSession.StartedAt != nil {
			startedAt = *firstSession.StartedAt
		}
		if firstSession.EndedAt != nil {
			endedAt = *firstSession.EndedAt
		}
		totalComments = firstSession.TotalComments

		platforms, err := s.repo.ListPlatformsBySession(ctx, firstSession.ID)
		if err == nil && len(platforms) > 0 {
			platform = platforms[0].Platform
			platformLiveID = platforms[0].PlatformLiveID
		}
	}

	return LiveOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Platform:               platform,
		PlatformLiveID:         platformLiveID,
		Status:                 EffectiveStatus(event.Status, event.ScheduledAt, event.EndsAt),
		StartedAt:              &startedAt,
		EndedAt:                &endedAt,
		TotalComments:          totalComments,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		PixDiscountPercent:     event.PixDiscountPercent,
		ScheduledAt:            event.ScheduledAt,
		EndsAt:                 event.EndsAt,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,

		WaitlistNotifiedTTLMinutes: event.WaitlistNotifiedTTLMinutes,
	}, nil
}

// GetEventWithSessions returns an event with all its sessions and platforms
func (s *Service) GetEventWithSessions(ctx context.Context, id, storeID string) (EventOutput, error) {
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return EventOutput{}, err
	}

	if pct, err := s.repo.GetPixDiscountPercent(ctx, event.ID); err == nil {
		event.PixDiscountPercent = pct
	}

	// Get sessions for this event
	sessionRows, err := s.repo.ListSessionsByEvent(ctx, event.ID)
	if err != nil {
		return EventOutput{}, err
	}

	// Métrica das transmissões: UMA leitura por evento, não uma por sessão. A
	// GetSessionStats que morava aqui rodava dentro do laço (N+1) e ainda
	// creditava o carrinho inteiro à sessão em que ele nasceu.
	//
	// Falha aqui não derruba a tela do evento: o painel volta com os números
	// zerados, que é muito melhor que 500 no detalhe do evento inteiro.
	revenueBySession := map[string]SessionRevenueOutput{}
	if metrics, merr := s.GetSessionMetrics(ctx, event.ID, storeID); merr != nil {
		logger.From(ctx, s.logger).Warn("failed to get session metrics",
			zap.String("event_id", event.ID), zap.Error(merr))
	} else {
		for _, m := range metrics.Sessions {
			revenueBySession[m.SessionID] = m.SessionRevenueOutput
		}
	}

	// Quantos produtos cada transmissão libera, numa leitura só. Zero (sessão
	// ausente do mapa) é resposta legítima: "esta transmissão vende todos os
	// produtos ativos da loja". Sem esse número a tela não consegue distinguir
	// "vende tudo" de "esqueci de configurar" — e, como a lista vazia é o
	// default, a diferença é a barreira inteira.
	//
	// Best-effort como a métrica acima: contagem é enfeite de tela, e derrubar o
	// detalhe da campanha por causa dela seria trocar a venda pelo badge.
	productCountBySession := map[string]int{}
	if counts, cerr := s.repo.CountSessionProductsByEvent(ctx, event.ID); cerr != nil {
		logger.From(ctx, s.logger).Warn("failed to count session products",
			zap.String("event_id", event.ID), zap.Error(cerr))
	} else {
		productCountBySession = counts
	}

	// Build session outputs with platforms and stats
	sessions := make([]SessionOutput, len(sessionRows))
	for i, sessionRow := range sessionRows {
		platforms, err := s.repo.ListPlatformsBySession(ctx, sessionRow.ID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to list platforms for session",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
			platforms = []PlatformRow{}
		}

		platformOutputs := make([]PlatformOutput, len(platforms))
		for j, p := range platforms {
			platformOutputs[j] = PlatformOutput{
				ID:             p.ID,
				SessionID:      p.SessionID,
				Platform:       p.Platform,
				PlatformLiveID: p.PlatformLiveID,
				AddedAt:        p.AddedAt,
			}
		}

		// Get comments for this session (limit 100 for performance)
		var commentOutputs []CommentOutput
		commentRows, err := s.repo.ListCommentsBySession(ctx, sessionRow.ID, 100, 0)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to list comments for session",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
		} else {
			commentOutputs = make([]CommentOutput, len(commentRows))
			for k, c := range commentRows {
				commentOutputs[k] = CommentOutput{
					Handle: c.PlatformHandle,
					Text:   c.Text,
				}
			}
		}

		sessions[i] = SessionOutput{
			ID:                     sessionRow.ID,
			EventID:                sessionRow.EventID,
			Type:                   sessionRow.Type,
			Status:                 sessionRow.Status,
			SequenceOrder:          sessionRow.SequenceOrder,
			CurrentActiveProductID: sessionRow.CurrentActiveProductID,
			ProcessingPaused:       sessionRow.ProcessingPaused,
			StartedAt:              sessionRow.StartedAt,
			EndedAt:                sessionRow.EndedAt,
			TotalComments:          sessionRow.TotalComments,
			ProductCount:           productCountBySession[sessionRow.ID],
			SessionRevenueOutput:   revenueBySession[sessionRow.ID],
			Platforms:              platformOutputs,
			Comments:               commentOutputs,
			CreatedAt:              sessionRow.CreatedAt,
			UpdatedAt:              sessionRow.UpdatedAt,
		}
	}

	upsellCount, err := s.repo.CountEventUpsells(ctx, event.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to count event upsells", zap.String("event_id", event.ID), zap.Error(err))
	}

	return EventOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Status:                 event.Status,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		PixDiscountPercent:     event.PixDiscountPercent,
		ScheduledAt:            event.ScheduledAt,
		EndsAt:                 event.EndsAt,
		Description:            event.Description,
		UpsellCount:            upsellCount,
		Sessions:               sessions,

		WaitlistNotifiedTTLMinutes: event.WaitlistNotifiedTTLMinutes,
		CreatedAt:                  event.CreatedAt,
		UpdatedAt:                  event.UpdatedAt,
	}, nil
}

func (s *Service) List(ctx context.Context, input ListLivesInput) (ListLivesOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	lives, total, err := s.repo.ListLives(ctx, ListLivesParams{
		StoreID: input.StoreID,
		Search:  input.Search,
		Pagination: struct {
			Limit  int
			Offset int
		}{
			Limit:  input.Pagination.Limit,
			Offset: input.Pagination.Offset(),
		},
		Sorting: struct {
			SortBy    string
			SortOrder string
		}{
			SortBy:    input.Sorting.SortBy,
			SortOrder: input.Sorting.SortOrder,
		},
		Filters: input.Filters,
	})
	if err != nil {
		return ListLivesOutput{}, err
	}

	return ListLivesOutput{
		Lives:      lives,
		Total:      total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateLiveInput) (LiveOutput, error) {
	// Verify event exists and belongs to store
	current, err := s.repo.GetEventByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return LiveOutput{}, err
	}

	// RN-05/CA-05.7: a edição da janela. Antes deste ponto o PUT só trocava
	// título e desconto PIX — um ends_at errado era permanente.
	if input.Window.Touches() && current != nil {
		// O teto não pode ser removido: sem ends_at o carrinho perde o prazo.
		if input.Window.SetEndsAt && input.Window.EndsAt == nil {
			return LiveOutput{}, httpx.ErrBadRequest("endsAt não pode ser removido: o evento precisa de uma data de encerramento")
		}
		// Coerência com o valor que NÃO está sendo editado.
		endsAt := input.Window.EndsAt
		if !input.Window.SetEndsAt {
			endsAt = current.EndsAt
		}
		startsAt := input.Window.StartsAt
		if !input.Window.SetStartsAt {
			startsAt = current.ScheduledAt
		}
		if startsAt != nil && endsAt != nil && !endsAt.After(*startsAt) {
			return LiveOutput{}, httpx.ErrBadRequest("endsAt precisa ser depois de startsAt")
		}
		if err := s.applyEventWindow(ctx, input.ID, input.StoreID, input.Window, true); err != nil {
			return LiveOutput{}, err
		}
	}

	// Update event title
	event, err := s.repo.UpdateEventTitle(ctx, input.ID, input.Title)
	if err != nil {
		return LiveOutput{}, err
	}

	// Optional pix-discount update — clamp range defensively even though the
	// handler/validator already gates it.
	if input.PixDiscountPercent != nil {
		pct := *input.PixDiscountPercent
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		if err := s.repo.SetPixDiscountPercent(ctx, event.ID, input.StoreID, pct); err != nil {
			return LiveOutput{}, err
		}
	}

	// RN-10: janela extra do promovido da fila. O repo faz o clamp para 5..240,
	// espelhando o CHECK da 000073.
	if input.WaitlistNotifiedTTLMinutes != nil {
		if err := s.repo.SetWaitlistNotifiedTTLMinutes(ctx, event.ID, input.StoreID, *input.WaitlistNotifiedTTLMinutes); err != nil {
			return LiveOutput{}, err
		}
	}

	// Get full live output
	return s.GetByID(ctx, event.ID, input.StoreID)
}

// Start é o "Iniciar live" do painel. E37: ele ATIVA o evento, não só a sessão.
//
// Antes daqui só saía StartSession, e isso era um no-op de negócio: a sessão já
// nasce 'active' e todas as queries de resolução aceitam ('active','live'), de
// modo que o UPDATE só carimbava started_at. O evento continuava 'scheduled', a
// regra 4 de WindowAt continuava devolvendo WindowNotStarted e a ingestão
// respondia "a live começa em <data já vencida>" para sempre. O lojista clicava,
// recebia 200 e o badge não mudava — o botão mentia.
//
// Ativar antes de mexer na sessão é de propósito: se a sessão falhar (evento sem
// sessão, por exemplo) o lojista recebe erro com o evento já ativo, que é o
// estado que ele pediu; a ordem inversa deixaria a sessão iniciada num evento
// que não vende.
func (s *Service) Start(ctx context.Context, id, storeID string) (LiveOutput, error) {
	// Verify event exists and belongs to store. É esta checagem que autoriza
	// ActivateScheduledEvent a rodar sem store_id no WHERE.
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}
	if event == nil {
		return LiveOutput{}, httpx.ErrNotFound("event not found")
	}

	now := time.Now().UTC()
	if WindowAt(event.Status, event.ScheduledAt, event.EndsAt, now) == WindowEnded {
		// Sem este guard o botão ressuscitaria um evento encerrado: o sweep de
		// ends_at o fecharia de novo no ciclo seguinte e o lojista veria o
		// estado piscar. Antes daqui o pedido morria adiante com "no active
		// session found for event", que não explica nada.
		return LiveOutput{}, httpx.ErrUnprocessable("evento encerrado nao pode ser iniciado")
	}

	// Flip scheduled → active. Falso = já estava ativo: iniciar duas vezes é
	// no-op, não erro.
	activated, err := s.repo.ActivateScheduledEvent(ctx, id)
	if err != nil {
		return LiveOutput{}, err
	}
	if activated {
		logger.From(ctx, s.logger).Info("scheduled event activated by merchant",
			zap.String("event_id", id),
		)
	}

	// "Iniciar" com a hora marcada ainda no futuro ANTECIPA a janela. Só flipar
	// o status deixaria o evento 'active' e mesmo assim sem vender — a regra 2
	// de WindowAt continua barrando enquanto starts_at não chega —, ou seja, o
	// botão voltaria a mentir por outro motivo. Reusa applyEventWindow, o ponto
	// único de escrita da janela; ends_at não é tocado, então não há task de
	// fechamento para mover.
	if event.ScheduledAt != nil && event.ScheduledAt.After(now) {
		if err := s.applyEventWindow(ctx, id, storeID, EventWindowUpdate{
			SetStartsAt: true, StartsAt: &now,
		}, false); err != nil {
			return LiveOutput{}, err
		}
		logger.From(ctx, s.logger).Info("event start pulled forward by merchant",
			zap.String("event_id", id),
			zap.Time("was_scheduled_at", *event.ScheduledAt),
		)
	}

	// Iniciar a CAMPANHA não depende de existir transmissão.
	//
	// Campanha sem transmissão nenhuma é um estado legítimo desde que a sessão
	// automática saiu da criação: o lojista marca a "Semana Black" e pendura as
	// lives depois. Aqui isso era um erro seco — "no active session found for
	// event" — e o botão "Iniciar" morria para toda campanha recém-criada.
	//
	// O que se inicia é a janela do evento, escrita logo acima. A transmissão,
	// quando existe, é iniciada junto por conveniência.
	session, err := s.repo.GetActiveSessionByEvent(ctx, id)
	if err != nil {
		return LiveOutput{}, err
	}
	if session != nil {
		if _, err := s.repo.StartSession(ctx, session.ID); err != nil {
			return LiveOutput{}, err
		}
	}

	return s.GetByID(ctx, id, storeID)
}

func (s *Service) End(ctx context.Context, input EndLiveInput) (EndLiveOutput, error) {
	// 0. Idempotency guard: if the event was already ended, do not re-run
	// the finalization side-effects (carts, DMs). Return the current state.
	existing, err := s.repo.GetEventByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}
	if existing != nil && existing.Status == "ended" {
		liveOutput, _ := s.GetByID(ctx, existing.ID, input.StoreID)
		return EndLiveOutput{
			Live:           liveOutput,
			CartsFinalized: 0,
			AutoSendLinks:  false,
		}, nil
	}

	// 1. End the event
	event, err := s.repo.EndEvent(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}

	// 2. End all active sessions for this event
	sessions, err := s.repo.ListSessionsByEvent(ctx, input.ID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list sessions for event",
			zap.String("event_id", input.ID),
			zap.Error(err),
		)
	} else {
		for _, session := range sessions {
			if session.Status == "active" || session.Status == "live" {
				_, err := s.repo.EndSession(ctx, session.ID)
				if err != nil {
					logger.From(ctx, s.logger).Error("failed to end session",
						zap.String("session_id", session.ID),
						zap.Error(err),
					)
				}
			}
		}
	}

	// 3. Finalize all pending carts (now tied to event)
	cartsFinalized, err := s.repo.FinalizeCartsByEvent(ctx, input.ID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to finalize carts",
			zap.String("event_id", input.ID),
			zap.Error(err),
		)
	}

	// Capture the slug before spawning detached goroutines: the request ctx is
	// recycled by fasthttp when the response is sent.
	storeSlug, _ := ctx.Value(logger.StoreSlugKey).(string)

	// 4. Determine if we should auto-send checkout links.
	// Carts are unique per (event_id, platform_user_id), so multi-session
	// events already aggregate items per buyer. Use the explicit override
	// when provided, otherwise fall back to the store default.
	sessionCount, _ := s.repo.CountSessionsByEvent(ctx, input.ID)
	autoSend := false
	if input.AutoSend != nil {
		autoSend = *input.AutoSend
	} else {
		storeDefault, err := s.repo.GetStoreAutoSendSetting(ctx, input.StoreID)
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to get store auto_send setting",
				zap.Error(err),
			)
		} else {
			autoSend = storeDefault
		}
	}

	logger.From(ctx, s.logger).Info("live event ended",
		zap.String("event_id", input.ID),
		zap.Int("session_count", sessionCount),
		zap.Int("carts_finalized", cartsFinalized),
		zap.Bool("auto_send_links", autoSend),
	)

	// 5. Dispatch checkout DMs (best-effort, async — never blocks the response).
	if autoSend && s.notifier != nil {
		go s.sendCheckoutLinksForEvent(logger.WithStore(context.Background(), input.StoreID, storeSlug), input.StoreID, event.ID)
	}

	// Get full output
	liveOutput, _ := s.GetByID(ctx, event.ID, input.StoreID)

	return EndLiveOutput{
		Live:           liveOutput,
		CartsFinalized: cartsFinalized,
		AutoSendLinks:  autoSend,
	}, nil
}

// sendCheckoutLinksForEvent iterates over all carts of an event with at least
// one item and dispatches a checkout DM per buyer through the configured
// Notifier. Errors are logged individually and never interrupt the loop.
func (s *Service) sendCheckoutLinksForEvent(ctx context.Context, storeID, eventID string) {
	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list carts for event checkout dispatch",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return
	}

	// O título da campanha é a variável {evento} da mensagem. Uma leitura só,
	// fora do laço: é o mesmo evento para todos os carrinhos.
	eventTitle := ""
	if ev, err := s.repo.GetEventByID(ctx, eventID, storeID); err == nil && ev != nil {
		eventTitle = ev.Title
	}

	sent := 0
	skipped := 0
	undelivered := 0
	for _, c := range carts {
		if c.TotalItems <= 0 || c.PlatformUserID == "" {
			skipped++
			continue
		}
		// Only notify carts that were just finalized for checkout. Skip
		// carts already paid, expired, or abandoned during the live.
		if c.Status != "checkout" {
			skipped++
			continue
		}
		if c.PaymentStatus != nil && *c.PaymentStatus == "paid" {
			skipped++
			continue
		}
		target, _ := s.repo.GetLatestReplyTarget(ctx, eventID, c.PlatformUserID)
		res, err := s.notifier.NotifyEventCheckout(ctx, NotifyEventCheckoutParams{
			StoreID:          storeID,
			EventID:          eventID,
			EventTitle:       eventTitle,
			CartID:           c.ID,
			CartToken:        c.Token,
			PlatformUserID:   c.PlatformUserID,
			PlatformHandle:   c.PlatformHandle,
			CommentID:        target.CommentID,
			CommentCreatedAt: target.CreatedAt,
			DeadlineAt:       c.ExpiresAt,
			TotalItems:       c.TotalItems,
			TotalValue:       c.TotalValue,
		})
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to notify event checkout",
				zap.String("event_id", eventID),
				zap.String("cart_id", c.ID),
				zap.String("platform_user_id", c.PlatformUserID),
				zap.Error(err),
			)
			continue
		}
		if !res.Delivered {
			// RN-38 — a janela do Instagram já tinha fechado para esta pessoa.
			// Não é falha: está registrado com motivo e aparece na lista de
			// "compradores não avisados" do evento. Contar como enviado seria
			// mentir para o próprio log.
			undelivered++
			continue
		}
		sent++
	}

	logger.From(ctx, s.logger).Info("event checkout dispatch finished",
		zap.String("event_id", eventID),
		zap.Int("sent", sent),
		zap.Int("skipped", skipped),
		zap.Int("undelivered", undelivered),
		zap.Int("total", len(carts)),
	)
}

func (s *Service) Delete(ctx context.Context, id, storeID string) error {
	return s.repo.DeleteEvent(ctx, id, storeID)
}

func (s *Service) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

// =============================================================================
// SESSION OPERATIONS
// =============================================================================

// CreateSession creates a new session within an event.
// Uses a transaction to ensure atomicity of session + platform creation.
//
// NÃO há gate de status do evento aqui, e é de propósito: pendurar a live de
// segunda numa campanha AGENDADA é o caso de uso central do guarda-chuva
// ("marco a Semana Black hoje, escolho as transmissões depois"). Encerrado
// também passa — a sessão nasce sem capturar nada, e recusar seria impedir o
// lojista de registrar o que já aconteceu.
func (s *Service) CreateSession(ctx context.Context, input CreateSessionInput) (CreateSessionOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return CreateSessionOutput{}, err
	}

	// Meia mídia não existe: platform sem id (ou id sem platform) gravaria uma
	// linha de live_session_platforms que não resolve comentário nenhum e
	// mentiria na tela dizendo que a sessão está vinculada. Ou vêm os dois, ou
	// a sessão nasce sem mídia.
	if (input.Platform == "") != (input.PlatformLiveID == "") {
		return CreateSessionOutput{}, httpx.DomainError(400, httpx.CodeSessionMediaIncomplete,
			"platform e platformLiveId precisam vir juntos: mande os dois para vincular a publicação, ou nenhum para criar a transmissão sem publicação")
	}

	// Create the session and add the platform in a single transaction.
	//
	// A sessão nasce VAZIA e, portanto, vendendo todos os produtos ativos da
	// loja. A herança da lista do evento saiu junto com a lista do evento: ela
	// existia para o caso "configurei os produtos na campanha e criei a sessão
	// depois", que não existe mais — não há lista de campanha de onde herdar, e
	// cada transmissão é configurada explicitamente.
	session, platform, err := s.repo.CreateSessionWithPlatformTx(ctx, input.EventID, SessionTypeFromEventType(input.Type), input.Platform, input.PlatformLiveID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to create session with platform",
			zap.String("event_id", input.EventID),
			zap.Error(err),
		)
		return CreateSessionOutput{}, err
	}

	// Os metadados da publicação, gravados na MÍDIA — o mesmo SetMedia que
	// CreatePostEvent chama. Sem isto, a mesma publicação aparecia com
	// permalink/capa/legenda quando virava evento novo e sem nada quando virava
	// sessão de um evento existente.
	//
	// Best-effort, como no CreatePostEvent: metadado é enfeite de tela, e
	// derrubar a criação da transmissão por causa dele seria trocar a venda
	// pela miniatura.
	if input.PlatformLiveID != "" && (input.MediaPermalink != "" || input.MediaThumbnailURL != "" || input.MediaCaption != "") {
		if err := s.repo.SetMedia(ctx, PostMediaInput{
			MediaID:      input.PlatformLiveID,
			Permalink:    input.MediaPermalink,
			ThumbnailURL: input.MediaThumbnailURL,
			Caption:      input.MediaCaption,
		}); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set session media metadata",
				zap.String("session_id", session.ID), zap.Error(err))
		}
	}

	logger.From(ctx, s.logger).Info("session created",
		zap.String("event_id", input.EventID),
		zap.String("session_id", session.ID),
		zap.String("platform", input.Platform),
	)

	out := CreateSessionOutput{
		ID:        session.ID,
		EventID:   session.EventID,
		Type:      session.Type,
		Status:    session.Status,
		CreatedAt: session.CreatedAt,
	}
	if platform != nil {
		out.Platform = &PlatformOutput{
			ID:             platform.ID,
			SessionID:      platform.SessionID,
			Platform:       platform.Platform,
			PlatformLiveID: platform.PlatformLiveID,
			AddedAt:        platform.AddedAt,
		}
	}
	return out, nil
}

// GetSessionByPlatformLiveID resolves the session that owns a media id.
//
// D18/D20: deixou de filtrar por status. Devolver a sessão encerrada é o que
// permite responder ao comprador em vez de descartar o comentário em silêncio;
// quem decide se ela ainda vende é SessionAcceptsPurchase.
// GetLatestReplyTarget expõe o comentário respondível de um comprador para os
// caminhos ASSÍNCRONOS de fora deste pacote (fim da fila, por exemplo).
//
// Sem ele, uma mensagem disparada por task só teria o IGSID — e um DM por IGSID
// sem janela aberta é recusado pelo Instagram (2534022). Ou seja: o gatilho
// nasceria sem caminho de entrega e todo envio viraria linha "não entregue".
func (s *Service) GetLatestReplyTarget(ctx context.Context, eventID, platformUserID string) (ReplyTarget, error) {
	return s.repo.GetLatestReplyTarget(ctx, eventID, platformUserID)
}

// MarkSessionPublished amarra a transmissão recém-criada ao agendamento que a
// publicou (RN-31): resolve a sessão pela mídia e grava live_sessions.publish_at.
//
// Devolve o id da sessão porque quem chama (o disparo do agendamento) precisa
// dele para fechar o job — e é a mesma resolução, feita uma vez só.
//
// A escrita de publish_at existe para não haver duas fontes da verdade sobre
// "quando publica". A coluna entrou na 000114 e passou o épico inteiro sem
// escritor; se o agendador guardasse a hora apenas em session_publish_jobs, a
// sessão continuaria mentindo que nunca foi agendada.
func (s *Service) MarkSessionPublished(ctx context.Context, mediaID string, publishAt time.Time) (string, error) {
	session, err := s.repo.GetSessionByPlatformLiveID(ctx, mediaID)
	if err != nil {
		return "", err
	}
	if session == nil {
		return "", nil
	}
	if err := s.repo.SetSessionPublishAt(ctx, session.ID, publishAt); err != nil {
		return session.ID, err
	}
	return session.ID, nil
}

func (s *Service) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*SessionOutput, error) {
	session, err := s.repo.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, nil
	}

	// Get platforms for this session
	platforms, err := s.repo.ListPlatformsBySession(ctx, session.ID)
	if err != nil {
		return nil, err
	}

	platformOutputs := make([]PlatformOutput, len(platforms))
	for i, p := range platforms {
		platformOutputs[i] = PlatformOutput{
			ID:             p.ID,
			SessionID:      p.SessionID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return &SessionOutput{
		ID:                     session.ID,
		EventID:                session.EventID,
		Type:                   session.Type,
		Status:                 session.Status,
		CurrentActiveProductID: session.CurrentActiveProductID,
		ProcessingPaused:       session.ProcessingPaused,
		StartedAt:              session.StartedAt,
		EndedAt:                session.EndedAt,
		TotalComments:          session.TotalComments,
		Platforms:              platformOutputs,
		CreatedAt:              session.CreatedAt,
		UpdatedAt:              session.UpdatedAt,
	}, nil
}

// GetEventByPlatformLiveID resolves the event that owns a media id.
//
// D19/D20: deixou de filtrar por status — campanha agendada e campanha
// encerrada agora resolvem. O gate de janela (WindowAt) decide o resto.
func (s *Service) GetEventByPlatformLiveID(ctx context.Context, platformLiveID string) (*EventOutput, error) {
	event, err := s.repo.GetEventByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, nil
	}

	return &EventOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Status:                 event.Status,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		ScheduledAt:            event.ScheduledAt,
		EndsAt:                 event.EndsAt,
		Description:            event.Description,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,
	}, nil
}

// =============================================================================
// PLATFORM OPERATIONS
// =============================================================================

// AddPlatform adds a platform ID to a session.
func (s *Service) AddPlatform(ctx context.Context, input AddPlatformInput) (PlatformOutput, error) {
	row, err := s.repo.AddPlatformToSession(ctx, input.SessionID, input.Platform, input.PlatformLiveID)
	if err != nil {
		return PlatformOutput{}, err
	}

	logger.From(ctx, s.logger).Info("platform added to session",
		zap.String("session_id", input.SessionID),
		zap.String("platform", input.Platform),
		zap.String("platform_live_id", input.PlatformLiveID),
	)

	return PlatformOutput{
		ID:             row.ID,
		SessionID:      row.SessionID,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt,
	}, nil
}

// LinkSessionMedia vincula a publicação a UMA sessão nomeada.
//
// AddPlatform (acima) resolve a sessão sozinho, pela mais recente que estiver no
// ar. Isso bastava quando um evento era uma transmissão só — é a rota de crash
// recovery de live. Numa campanha guarda-chuva ela escreveria a mídia na sessão
// errada em silêncio, que é o pior desfecho possível: o comentário continua sem
// virar carrinho e o painel jura que está vinculado.
//
// Esta versão exige o sessionId e checa a posse (sessão → evento → loja) antes
// de gravar. É o "vincular depois" que a criação de sessão sem mídia prometia.
func (s *Service) LinkSessionMedia(ctx context.Context, input LinkSessionMediaInput) (PlatformOutput, error) {
	if err := s.resolveSessionOfEvent(ctx, input.SessionID, input.EventID, input.StoreID); err != nil {
		return PlatformOutput{}, err
	}

	// A espécie da transmissão só é conhecida agora. A campanha é criada sem
	// perguntá-la — na hora de criar, ninguém sabe ainda se aquilo vai ser uma
	// live, um post ou um reel —, então a sessão nasce como marcador e aprende
	// o que é quando a publicação chega. Vazio mantém o que já estava lá.
	if input.Type != "" {
		if err := s.repo.SetSessionType(ctx, input.SessionID, input.Type); err != nil {
			return PlatformOutput{}, fmt.Errorf("setting session type on media link: %w", err)
		}
	}


	row, err := s.repo.AddPlatformToSession(ctx, input.SessionID, input.Platform, input.PlatformLiveID)
	if err != nil {
		return PlatformOutput{}, err
	}

	// Best-effort, como em CreateSession e CreatePostEvent: metadado é enfeite
	// de tela, e derrubar o vínculo por causa da miniatura trocaria a venda
	// pela capa.
	if input.MediaPermalink != "" || input.MediaThumbnailURL != "" || input.MediaCaption != "" {
		if err := s.repo.SetMedia(ctx, PostMediaInput{
			MediaID:      input.PlatformLiveID,
			Permalink:    input.MediaPermalink,
			ThumbnailURL: input.MediaThumbnailURL,
			Caption:      input.MediaCaption,
		}); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set linked media metadata",
				zap.String("session_id", input.SessionID), zap.Error(err))
		}
	}

	logger.From(ctx, s.logger).Info("media linked to session",
		zap.String("event_id", input.EventID),
		zap.String("session_id", input.SessionID),
		zap.String("platform", input.Platform),
		zap.String("platform_live_id", input.PlatformLiveID),
	)

	return PlatformOutput{
		ID:             row.ID,
		SessionID:      row.SessionID,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt,
	}, nil
}

// ListPlatforms returns all platforms for a session.
func (s *Service) ListPlatforms(ctx context.Context, sessionID string) ([]PlatformOutput, error) {
	platforms, err := s.repo.ListPlatformsBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	outputs := make([]PlatformOutput, len(platforms))
	for i, p := range platforms {
		outputs[i] = PlatformOutput{
			ID:             p.ID,
			SessionID:      p.SessionID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return outputs, nil
}

// RemovePlatform removes a platform from a session.
func (s *Service) RemovePlatform(ctx context.Context, sessionID, platformLiveID string) error {
	return s.repo.RemovePlatformFromSession(ctx, sessionID, platformLiveID)
}

// =============================================================================
// CART OPERATIONS
// =============================================================================

// AddToCart adds a product to a user's cart during a live event.
// Creates a new cart if one doesn't exist for this user in this event.
func (s *Service) AddToCart(ctx context.Context, input AddToCartInput) (AddToCartOutput, error) {
	// Generate token for new carts
	token, err := generateCartToken()
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("generating cart token: %w", err)
	}

	// Resolve customer for this cart so the Customers tab reflects activity
	// in real time. Best-effort: a failure here must not block adding to cart
	// (the cart still works, the customer link is just missing).
	customerID := input.CustomerID
	if customerID == nil && s.customerUpserter != nil && input.StoreID != "" && input.PlatformUserID != "" {
		id, upsertErr := s.customerUpserter.UpsertForCart(ctx, input.StoreID, input.PlatformUserID, input.PlatformHandle)
		if upsertErr != nil {
			logger.From(ctx, s.logger).Warn("failed to upsert customer for cart",
				zap.String("store_id", input.StoreID),
				zap.String("platform_user_id", input.PlatformUserID),
				zap.Error(upsertErr),
			)
		} else if id != "" {
			customerID = &id
		}
	}

	// Get or create cart for this user in this event. Thread the session so a
	// new cart records its originating session.
	var originSession *string
	if input.SessionID != "" {
		originSession = &input.SessionID
	}
	cart, isNew, err := s.repo.GetOrCreateCart(ctx, GetOrCreateCartParams{
		EventID:        input.EventID,
		SessionID:      originSession,
		PlatformUserID: input.PlatformUserID,
		PlatformHandle: input.PlatformHandle,
		Token:          token,
		CustomerID:     customerID,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("getting or creating cart: %w", err)
	}

	// Add item to cart, attributed to the session it was added in (first-touch).
	err = s.repo.AddCartItem(ctx, AddCartItemParams{
		CartID:             cart.ID,
		ProductID:          input.ProductID,
		SessionID:          input.SessionID,
		Quantity:           input.Quantity,
		UnitPrice:          input.ProductPrice,
		WaitlistedQuantity: input.WaitlistedQuantity,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("adding item to cart: %w", err)
	}

	// Get updated cart totals
	totalItems, totalCents, err := s.repo.GetCartTotals(ctx, cart.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get cart totals", zap.Error(err))
		// Continue with zero totals - notification can still be sent
	}

	logger.From(ctx, s.logger).Info("added product to cart",
		zap.String("cart_id", cart.ID),
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
		zap.Int("quantity", input.Quantity),
		zap.Bool("new_cart", isNew),
		zap.Int("total_items", totalItems),
		zap.Int64("total_cents", totalCents),
	)

	return AddToCartOutput{
		CartID:     cart.ID,
		CartToken:  cart.Token,
		IsNewCart:  isNew,
		TotalItems: totalItems,
		TotalCents: totalCents,
	}, nil
}

// generateCartToken creates a random token for cart checkout URLs.
func generateCartToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing
// =============================================================================

// GetEventStats returns stats for an event (comments, carts, revenue).
func (s *Service) GetEventStats(ctx context.Context, eventID, storeID string) (EventStatsOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return EventStatsOutput{}, err
	}

	stats, err := s.repo.GetEventStats(ctx, eventID)
	if err != nil {
		return EventStatsOutput{}, err
	}

	return EventStatsOutput{
		TotalComments:     stats.TotalComments,
		TotalCarts:        stats.TotalCarts,
		OpenCarts:         stats.OpenCarts,
		CheckoutCarts:     stats.CheckoutCarts,
		PaidCarts:         stats.PaidCarts,
		TotalProductsSold: stats.TotalProductsSold,
		ProjectedRevenue:  stats.ProjectedRevenue,
		ConfirmedRevenue:  stats.ConfirmedRevenue,
	}, nil
}

// GetSessionMetrics devolve a métrica do evento quebrada por transmissão —
// a Fatia 5 (RN-12/RN-13/RN-29).
//
// Contrato que o consumidor pode confiar: a soma de Sessions + Unattributed é
// EXATAMENTE ConfirmedRevenue e ProjectedRevenue, e esses dois batem com o
// confirmed/projected de GetEventStats. Não é coincidência nem arredondamento:
//   - confirmado — order_items é materializado por AllocateBySession com o
//     unit_price do carrinho, logo SUM(qty*price) por sessão reconstrói
//     orders.total_cents;
//   - projetado  — a mesma AllocateBySession roda aqui sobre os mesmos carrinhos
//     que GetEventStats.projected_revenue soma.
//
// Toda sessão do evento aparece, inclusive as que não venderam nada — "a live de
// terça faturou zero" é uma resposta, sumir da lista não é.
func (s *Service) GetSessionMetrics(ctx context.Context, eventID, storeID string) (EventSessionMetricsOutput, error) {
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return EventSessionMetricsOutput{}, err
	}

	sessions, err := s.repo.ListSessionsByEvent(ctx, eventID)
	if err != nil {
		return EventSessionMetricsOutput{}, err
	}

	confirmed, err := s.repo.ListSessionConfirmedRevenueByEvent(ctx, eventID)
	if err != nil {
		return EventSessionMetricsOutput{}, err
	}

	items, additions, err := s.repo.ListProjectionInputByEvent(ctx, eventID)
	if err != nil {
		return EventSessionMetricsOutput{}, err
	}
	projected := ProjectBySession(items, additions)

	revenue := map[string]*SessionRevenueOutput{}
	pick := func(sessionID string) *SessionRevenueOutput {
		r := revenue[sessionID]
		if r == nil {
			r = &SessionRevenueOutput{}
			revenue[sessionID] = r
		}
		return r
	}
	for _, c := range confirmed {
		r := pick(c.SessionID)
		r.PaidCarts, r.SoldUnits, r.ConfirmedRevenue = c.PaidCarts, c.SoldUnits, c.RevenueCents
	}
	for _, p := range projected {
		r := pick(p.SessionID)
		r.OpenCarts, r.ProjectedUnits, r.ProjectedRevenue = p.OpenCarts, p.Units, p.RevenueCents
	}

	out := EventSessionMetricsOutput{EventID: eventID}

	// O marcador de corte (D26). Ele não participa de nenhuma soma: existe para
	// a tela conseguir dizer "antes desta data, 'receita da live de terça'
	// significava outra coisa". Sem ele, a primeira comparação entre os dois
	// lados do corte vira um chamado de bug.
	if cut, err := s.repo.GetMetricCutover(ctx, MetricCutoverSessionAttribution); err != nil {
		logger.From(ctx, s.logger).Warn("failed to read attribution cutover marker",
			zap.String("event_id", eventID), zap.Error(err))
	} else if cut != nil {
		at := cut.EffectiveAt
		out.AttributionCutoverAt = &at
		out.AttributionCutoverNote = cut.Note
	}

	// As sessões, na ordem da campanha. ListSessionsByEvent devolve por
	// created_at DESC (a ordem da tela); relatório se lê da 1ª para a última.
	rows := make([]SessionMetricsOutput, 0, len(sessions))
	for _, sess := range sessions {
		row := SessionMetricsOutput{
			SessionID:         sess.ID,
			SequenceOrder:     sess.SequenceOrder,
			Type:              sess.Type,
			Status:            sess.Status,
			AttributionSource: sess.AttributionSource,
		}
		if r := revenue[sess.ID]; r != nil {
			row.SessionRevenueOutput = *r
		}
		rows = append(rows, row)
		delete(revenue, sess.ID)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SequenceOrder < rows[j].SequenceOrder })
	out.Sessions = rows

	// O que sobrou no mapa é receita sem sessão viva. O balde "" é o caso
	// normal (adição pelo painel, ou carrinho anterior ao log). Um id que não
	// está mais em live_sessions só apareceria se a sessão fosse apagada com
	// pedido selado — o ON DELETE SET NULL das FKs impede, mas se acontecer o
	// valor cai aqui em vez de sumir da soma.
	var unattributed SessionMetricsOutput
	var hasUnattributed bool
	for _, r := range revenue {
		hasUnattributed = true
		unattributed.PaidCarts += r.PaidCarts
		unattributed.SoldUnits += r.SoldUnits
		unattributed.ConfirmedRevenue += r.ConfirmedRevenue
		unattributed.OpenCarts += r.OpenCarts
		unattributed.ProjectedUnits += r.ProjectedUnits
		unattributed.ProjectedRevenue += r.ProjectedRevenue
	}
	if hasUnattributed {
		out.Unattributed = &unattributed
	}

	for _, row := range out.Sessions {
		out.ConfirmedRevenue += row.ConfirmedRevenue
		out.ProjectedRevenue += row.ProjectedRevenue
	}
	out.ConfirmedRevenue += unattributed.ConfirmedRevenue
	out.ProjectedRevenue += unattributed.ProjectedRevenue

	return out, nil
}

// ListCartsWithTotalByEvent returns all carts for an event with total value and item count.
func (s *Service) ListCartsWithTotalByEvent(ctx context.Context, eventID, storeID string) ([]CartWithTotalOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	outputs := make([]CartWithTotalOutput, len(carts))
	for i, cart := range carts {
		outputs[i] = CartWithTotalOutput{
			ID:              cart.ID,
			Token:           cart.Token,
			SessionID:       cart.SessionID,
			PlatformUserID:  cart.PlatformUserID,
			PlatformHandle:  cart.PlatformHandle,
			Status:          cart.Status,
			PaymentStatus:   cart.PaymentStatus,
			TotalValue:      cart.TotalValue,
			TotalItems:      cart.TotalItems,
			AvailableItems:  cart.AvailableItems,
			WaitlistedItems: cart.WaitlistedItems,
			CreatedAt:       cart.CreatedAt,
			ExpiresAt:       cart.ExpiresAt,
		}
	}

	return outputs, nil
}

// ResendCheckoutMessage re-sends the Instagram checkout DM for a single cart.
// It lets the merchant manually re-deliver the checkout link to a buyer from the
// event detail UI (for example, when the buyer missed the original message).
func (s *Service) ResendCheckoutMessage(ctx context.Context, eventID, cartID, storeID string) (CartWithTotalOutput, error) {
	if s.notifier == nil {
		return CartWithTotalOutput{}, httpx.DomainError(422, httpx.CodeIgNotifyNotConfigured, "instagram notifications are not configured")
	}

	// Verify event exists and belongs to store (authorization).
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return CartWithTotalOutput{}, err
	}

	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		return CartWithTotalOutput{}, err
	}

	var cart *CartWithTotalOutput
	for i := range carts {
		if carts[i].ID == cartID {
			c := CartWithTotalOutput{
				ID:              carts[i].ID,
				Token:           carts[i].Token,
				SessionID:       carts[i].SessionID,
				PlatformUserID:  carts[i].PlatformUserID,
				PlatformHandle:  carts[i].PlatformHandle,
				Status:          carts[i].Status,
				PaymentStatus:   carts[i].PaymentStatus,
				TotalValue:      carts[i].TotalValue,
				TotalItems:      carts[i].TotalItems,
				AvailableItems:  carts[i].AvailableItems,
				WaitlistedItems: carts[i].WaitlistedItems,
				CreatedAt:       carts[i].CreatedAt,
				ExpiresAt:       carts[i].ExpiresAt,
			}
			cart = &c
			break
		}
	}
	if cart == nil {
		return CartWithTotalOutput{}, httpx.ErrNotFound("cart not found")
	}
	if cart.PlatformUserID == "" {
		return CartWithTotalOutput{}, httpx.DomainError(422, httpx.CodeCartNoIgRecipient, "cart has no Instagram recipient")
	}
	if cart.TotalItems <= 0 {
		return CartWithTotalOutput{}, httpx.DomainError(422, httpx.CodeCartNoItemsToSend, "cart has no items to send")
	}

	// Prefer delivering via a private reply to the buyer's last comment (7-day
	// window) rather than a direct message by IGSID (24h window opened only by an
	// inbound DM). A comment does not open the DM window, so without this the
	// resend is rejected with error 2534022 even moments after the comment.
	target, lookupErr := s.repo.GetLatestReplyTarget(ctx, eventID, cart.PlatformUserID)
	if lookupErr != nil {
		// Don't abort — the DM path may still work — but make the failure visible
		// instead of silently degrading to DM-only (which fails outside the 24h
		// window with error 2534022).
		logger.From(ctx, s.logger).Error("failed to look up buyer's latest comment for private reply",
			zap.String("event_id", eventID),
			zap.String("platform_user_id", cart.PlatformUserID),
			zap.Error(lookupErr),
		)
	}
	logger.From(ctx, s.logger).Info("resend checkout message: delivery context",
		zap.String("event_id", eventID),
		zap.String("cart_id", cartID),
		zap.String("platform_user_id", cart.PlatformUserID),
		zap.String("comment_id", target.CommentID),
	)

	eventTitle := ""
	if ev, err := s.repo.GetEventByID(ctx, eventID, storeID); err == nil && ev != nil {
		eventTitle = ev.Title
	}

	res, err := s.notifier.NotifyEventCheckout(ctx, NotifyEventCheckoutParams{
		StoreID:          storeID,
		EventID:          eventID,
		EventTitle:       eventTitle,
		CartID:           cart.ID,
		CartToken:        cart.Token,
		PlatformUserID:   cart.PlatformUserID,
		PlatformHandle:   cart.PlatformHandle,
		CommentID:        target.CommentID,
		CommentCreatedAt: target.CreatedAt,
		DeadlineAt:       cart.ExpiresAt,
		TotalItems:       cart.TotalItems,
		TotalValue:       cart.TotalValue,
	})
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to resend checkout message",
			zap.String("event_id", eventID),
			zap.String("cart_id", cartID),
			zap.String("platform_user_id", cart.PlatformUserID),
			zap.String("comment_id", target.CommentID),
			zap.Error(err),
		)
		// Outside-the-window rejection (IG error 2534022): tell the merchant the
		// concrete fix instead of a generic failure.
		if strings.Contains(err.Error(), "2534022") {
			return CartWithTotalOutput{}, httpx.DomainError(422, httpx.CodeIgMessageWindowClosed,
				"O Instagram só permite enviar a mensagem se o comprador comentou recentemente ou mandou uma DM para a loja. Peça para o comprador comentar de novo na live (ou enviar uma DM) e clique em reenviar em seguida.")
		}
		return CartWithTotalOutput{}, httpx.DomainError(422, httpx.CodeIgMessageFailed, "failed to send Instagram message")
	}
	if !res.Delivered {
		// RN-38 — no reenvio MANUAL a não entrega tem de virar erro na tela: o
		// lojista acabou de clicar num botão e precisa saber que nada saiu, com
		// o motivo. No disparo em massa o mesmo fato é só registro, porque não
		// há ninguém esperando resposta.
		return CartWithTotalOutput{}, httpx.ErrUnprocessable(res.ReasonText)
	}

	logger.From(ctx, s.logger).Info("checkout message resent",
		zap.String("event_id", eventID),
		zap.String("cart_id", cartID),
		zap.String("platform_user_id", cart.PlatformUserID),
	)
	return *cart, nil
}

// ListCommentsByEvent returns the event's comments (with Instagram comment IDs)
// for the moderation UI. Validates store ownership of the event.
func (s *Service) ListCommentsByEvent(ctx context.Context, eventID, storeID string, limit, offset int) ([]CommentRow, error) {
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	return s.repo.ListCommentsByEvent(ctx, eventID, limit, offset)
}

// ListActiveCheckouts returns the carts in checkout phase for an event so the
// merchant can watch buyer activity in real time before payment lands.
func (s *Service) ListActiveCheckouts(ctx context.Context, eventID, storeID string) ([]ActiveCheckoutOutput, error) {
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	return s.repo.ListActiveCheckoutsByEvent(ctx, eventID)
}

// ListProductsByEvent returns all products sold in an event with quantity and revenue.
func (s *Service) ListProductsByEvent(ctx context.Context, eventID, storeID string) ([]EventProductSalesOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	products, err := s.repo.ListProductsByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	outputs := make([]EventProductSalesOutput, len(products))
	for i, product := range products {
		outputs[i] = EventProductSalesOutput{
			ID:            product.ID,
			Name:          product.Name,
			ImageURL:      product.ImageURL,
			Keyword:       product.Keyword,
			TotalQuantity: product.TotalQuantity,
			TotalRevenue:  product.TotalRevenue,
		}
	}

	return outputs, nil
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// O Modo Live é da SESSÃO (D17). As funções por EVENTO abaixo mantêm o painel
// atual de pé — ele só conhece o eventId — aplicando o estado em todas as
// sessões vivas do evento e lendo da sessão viva mais recente.

// SetActiveProduct define ou limpa o produto em destaque das sessões vivas do
// evento.
func (s *Service) SetActiveProduct(ctx context.Context, eventID, storeID string, productID *string) (*LiveModeStateOutput, error) {
	// Verify event exists and is active
	event, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	if event.Status != "active" {
		return nil, httpx.DomainError(400, httpx.CodeLiveEventNotActive, "can only set active product on active events")
	}

	if err := s.repo.SetActiveProductForEventSessions(ctx, eventID, storeID, productID); err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("active product updated",
		zap.String("event_id", eventID),
		zap.Stringp("product_id", productID),
	)

	return s.GetLiveModeState(ctx, eventID, storeID)
}

// SetProcessingPaused pausa ou retoma o processamento das sessões vivas do
// evento.
func (s *Service) SetProcessingPaused(ctx context.Context, eventID, storeID string, paused bool) (*LiveModeStateOutput, error) {
	// Verify event exists and is active
	event, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	if event.Status != "active" {
		return nil, httpx.DomainError(400, httpx.CodeLiveEventNotActive, "can only change processing state on active events")
	}

	if err := s.repo.SetProcessingPausedForEventSessions(ctx, eventID, storeID, paused); err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("processing paused state updated",
		zap.String("event_id", eventID),
		zap.Bool("paused", paused),
	)

	return s.GetLiveModeState(ctx, eventID, storeID)
}

// GetLiveModeState devolve o estado do evento lido da sessão viva mais recente.
func (s *Service) GetLiveModeState(ctx context.Context, eventID, storeID string) (*LiveModeStateOutput, error) {
	return s.repo.GetLiveModeState(ctx, eventID, storeID)
}

// =============================================================================
// MODO LIVE POR SESSÃO (D17)
// =============================================================================

// SetSessionActiveProduct define ou limpa o produto em destaque DAQUELA
// transmissão.
//
// O guard é o status da SESSÃO, não o do evento: num evento guarda-chuva de uma
// semana, exigir evento 'active' é justamente o acoplamento que a D17 desfaz.
func (s *Service) SetSessionActiveProduct(ctx context.Context, sessionID, eventID, storeID string, productID *string) (*LiveModeStateOutput, error) {
	if err := s.requireLiveSession(ctx, sessionID, eventID, storeID); err != nil {
		return nil, err
	}
	if _, err := s.repo.SetSessionActiveProduct(ctx, sessionID, storeID, productID); err != nil {
		return nil, err
	}
	logger.From(ctx, s.logger).Info("session active product updated",
		zap.String("session_id", sessionID),
		zap.Stringp("product_id", productID),
	)
	return s.repo.GetSessionLiveModeState(ctx, sessionID, storeID)
}

// SetSessionProcessingPaused pausa ou retoma o processamento DAQUELA
// transmissão — sem tocar nas outras sessões do mesmo evento.
func (s *Service) SetSessionProcessingPaused(ctx context.Context, sessionID, eventID, storeID string, paused bool) (*LiveModeStateOutput, error) {
	if err := s.requireLiveSession(ctx, sessionID, eventID, storeID); err != nil {
		return nil, err
	}
	if _, err := s.repo.SetSessionProcessingPaused(ctx, sessionID, storeID, paused); err != nil {
		return nil, err
	}
	logger.From(ctx, s.logger).Info("session processing paused state updated",
		zap.String("session_id", sessionID),
		zap.Bool("paused", paused),
	)
	return s.repo.GetSessionLiveModeState(ctx, sessionID, storeID)
}

// GetSessionLiveModeState devolve o estado do modo live DAQUELA transmissão.
func (s *Service) GetSessionLiveModeState(ctx context.Context, sessionID, eventID, storeID string) (*LiveModeStateOutput, error) {
	if err := s.resolveSessionOfEvent(ctx, sessionID, eventID, storeID); err != nil {
		return nil, err
	}
	return s.repo.GetSessionLiveModeState(ctx, sessionID, storeID)
}

// requireLiveSession confirma posse e exige que a transmissão esteja no ar —
// destacar produto ou pausar uma sessão encerrada não significa nada.
func (s *Service) requireLiveSession(ctx context.Context, sessionID, eventID, storeID string) error {
	if err := s.resolveSessionOfEvent(ctx, sessionID, eventID, storeID); err != nil {
		return err
	}
	session, err := s.repo.GetSessionByID(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.Status != "active" && session.Status != "live" {
		return httpx.ErrBadRequest("só é possível controlar o modo live de uma sessão em andamento")
	}
	return nil
}

// =============================================================================
// SESSION PRODUCTS — a lista de produtos vendáveis é da TRANSMISSÃO
//
// Não existe mais CRUD por EVENTO (AddEventProduct/ListEventProducts/
// UpdateEventProduct/DeleteEventProduct saíram). Uma live vende qualquer coisa,
// um post vende só o produto X e um story só o produto Y — e os três podem ser
// transmissões da mesma campanha, então "a lista da campanha" não tem resposta.
//
// Lista vazia = todos os produtos ativos da loja liberados naquela transmissão.
// =============================================================================

// resolveSessionOfEvent confirma que a sessão pertence ao evento e que o evento
// pertence à loja. É a checagem de posse que o CRUD por sessão precisa e que o
// CRUD por evento fazia com um GetEventByID só.
func (s *Service) resolveSessionOfEvent(ctx context.Context, sessionID, eventID, storeID string) error {
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return err
	}
	session, err := s.repo.GetSessionByID(ctx, sessionID)
	if err != nil {
		return err
	}
	if session == nil || session.EventID != eventID {
		return httpx.ErrNotFound("session not found")
	}
	return nil
}

// ListSessionProducts devolve a whitelist DAQUELA sessão. Vazia = vende tudo.
func (s *Service) ListSessionProducts(ctx context.Context, sessionID, eventID, storeID string) ([]SessionProductOutput, error) {
	if err := s.resolveSessionOfEvent(ctx, sessionID, eventID, storeID); err != nil {
		return nil, err
	}
	return s.repo.ListSessionProducts(ctx, sessionID)
}

// ListSessionWhitelist devolve a whitelist da sessão sem checagem de posse — é
// o caminho de INGESTÃO, onde a sessão já foi resolvida pela mídia que chegou
// no webhook.
func (s *Service) ListSessionWhitelist(ctx context.Context, sessionID string) ([]SessionProductOutput, error) {
	return s.repo.ListSessionProducts(ctx, sessionID)
}

// UpsertSessionProduct grava o produto na whitelist da sessão (POST e PUT são a
// mesma operação: a chave natural é (sessão, produto)).
func (s *Service) UpsertSessionProduct(ctx context.Context, input SessionProductInput) (SessionProductOutput, error) {
	if err := s.resolveSessionOfEvent(ctx, input.SessionID, input.EventID, input.StoreID); err != nil {
		return SessionProductOutput{}, err
	}
	output, err := s.repo.UpsertSessionProduct(ctx, input)
	if err != nil {
		return SessionProductOutput{}, err
	}
	logger.From(ctx, s.logger).Info("session whitelist product upserted",
		zap.String("session_id", input.SessionID),
		zap.String("product_id", input.ProductID),
	)
	return output, nil
}

// DeleteSessionProduct remove o produto da whitelist da sessão.
func (s *Service) DeleteSessionProduct(ctx context.Context, sessionID, eventID, storeID, productID string) error {
	if err := s.resolveSessionOfEvent(ctx, sessionID, eventID, storeID); err != nil {
		return err
	}
	if err := s.repo.DeleteSessionProduct(ctx, sessionID, productID); err != nil {
		return err
	}
	logger.From(ctx, s.logger).Info("session whitelist product removed",
		zap.String("session_id", sessionID),
		zap.String("product_id", productID),
	)
	return nil
}

// =============================================================================
// EVENT UPSELLS
// =============================================================================

// AddEventUpsell adds an upsell to an event
func (s *Service) AddEventUpsell(ctx context.Context, input AddEventUpsellInput) (EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	output, err := s.repo.AddEventUpsell(ctx, input)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	logger.From(ctx, s.logger).Info("added upsell to event",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
	)

	return output, nil
}

// ListEventUpsells returns all upsells for an event
func (s *Service) ListEventUpsells(ctx context.Context, eventID, storeID string) ([]EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	return s.repo.ListEventUpsells(ctx, eventID)
}

// UpdateEventUpsell updates an upsell's configuration
func (s *Service) UpdateEventUpsell(ctx context.Context, input UpdateEventUpsellInput) (EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	output, err := s.repo.UpdateEventUpsell(ctx, input)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	logger.From(ctx, s.logger).Info("updated event upsell",
		zap.String("event_id", input.EventID),
		zap.String("upsell_id", input.ID),
	)

	return output, nil
}

// DeleteEventUpsell removes an upsell from an event
func (s *Service) DeleteEventUpsell(ctx context.Context, id, eventID, storeID string) error {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return err
	}

	if err := s.repo.DeleteEventUpsell(ctx, id, eventID); err != nil {
		return err
	}

	logger.From(ctx, s.logger).Info("deleted event upsell",
		zap.String("event_id", eventID),
		zap.String("upsell_id", id),
	)

	return nil
}

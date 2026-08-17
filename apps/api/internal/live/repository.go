package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
)

type Repository struct {
	q    *sqlc.Queries
	pool *pgxpool.Pool
}

func NewRepository(q *sqlc.Queries, pool *pgxpool.Pool) *Repository {
	return &Repository{q: q, pool: pool}
}

// CreateSessionWithPlatformTx cria a sessão, registra a mídia e grava a lista de
// produtos numa transação só. sessionType é o tipo da transmissão (D3):
// live|post|reel|story.
//
// productIDs VAZIO é o caso normal: a sessão nasce sem lista e, sem lista, vende
// TODOS os produtos ativos da loja. Não há herança da campanha — a lista é da
// transmissão, então não há de onde herdar.
//
// productIDs PREENCHIDO é o atalho de publicar: quem publica um post/reel pelo
// LiveCart escolhe ali mesmo o que aquela publicação vende, e a lista tem de
// nascer JUNTO com a sessão. Fora da transação, uma falha ao gravar produto
// deixaria a publicação no ar com lista parcial — ou vazia, que sob "vazia vende
// tudo" libera o catálogo inteiro, o oposto do que o lojista escolheu.
// AllProductsBelongToStore diz se TODOS os ids informados sao produtos desta
// loja.
//
// A whitelist da transmissao grava (session_id, product_id) confiando na FK, que
// garante existencia e nao posse. Enquanto a lista so entrava um a um por rota
// autenticada, o buraco era teorico; a criacao de sessao passou a aceitar a
// lista inteira de uma vez, e sem esta checagem um uuid de outra loja entraria
// na lista de venda desta.
//
// Lista vazia devolve true: vazia significa "vende qualquer produto ativo da
// loja", e nao ha o que conferir.
func (r *Repository) AllProductsBelongToStore(ctx context.Context, storeID string, productIDs []string) (bool, error) {
	if len(productIDs) == 0 {
		return true, nil
	}
	sID, err := parseUUID(storeID)
	if err != nil {
		return false, err
	}
	ids := make([]pgtype.UUID, 0, len(productIDs))
	for _, raw := range productIDs {
		pid, err := parseUUID(raw)
		if err != nil {
			// Id malformado nao pertence a loja nenhuma; recusar aqui poupa uma
			// ida ao banco e devolve o mesmo 422 do id de outra loja.
			return false, nil
		}
		ids = append(ids, pid)
	}
	n, err := r.q.CountProductsOwnedByStore(ctx, sqlc.CountProductsOwnedByStoreParams{
		StoreID:    sID,
		ProductIds: ids,
	})
	if err != nil {
		return false, fmt.Errorf("counting products owned by store: %w", err)
	}
	return int(n) == len(ids), nil
}

func (r *Repository) CreateSessionWithPlatformTx(ctx context.Context, eventID, sessionType, platform, platformLiveID string, productIDs []string) (SessionRow, *PlatformRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return SessionRow{}, nil, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SessionRow{}, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// Create the session
	sessionRow, err := qtx.CreateLiveSession(ctx, sqlc.CreateLiveSessionParams{
		EventID: eventUID,
		Status:  "active",
		Type:    SessionTypeFromEventType(sessionType),
	})
	if err != nil {
		return SessionRow{}, nil, fmt.Errorf("creating live session: %w", err)
	}

	// A lista de produtos DESTA transmissão, na mesma transação em que ela
	// nasce. Mesma regra de CreateEventWithSessionTx: ou grava tudo, ou a
	// sessão não existe.
	for i, productID := range productIDs {
		productUID, err := parseUUID(productID)
		if err != nil {
			return SessionRow{}, nil, err
		}
		if _, err := qtx.UpsertSessionProduct(ctx, sqlc.UpsertSessionProductParams{
			SessionID:    sessionRow.ID,
			ProductID:    productUID,
			DisplayOrder: int32(i),
		}); err != nil {
			return SessionRow{}, nil, fmt.Errorf("adding product %s to the session product list: %w", productID, err)
		}
	}

	// Add the platform to the session — só quando a mídia já é conhecida, o
	// mesmo ramo que CreateEventWithSessionTx tem desde sempre. Sessão sem
	// plataforma é estado legítimo (D1: dá para criar a transmissão ANTES de a
	// mídia existir); ela simplesmente não captura nada até ser vinculada.
	var platformOut *PlatformRow
	if platform != "" && platformLiveID != "" {
		platformRow, err := qtx.AddPlatformToSession(ctx, sqlc.AddPlatformToSessionParams{
			SessionID:      sessionRow.ID,
			Platform:       platform,
			PlatformLiveID: platformLiveID,
		})
		if err != nil {
			return SessionRow{}, nil, fmt.Errorf("adding platform to session: %w", err)
		}
		platformOut = &PlatformRow{
			ID:             platformRow.ID.String(),
			SessionID:      platformRow.SessionID.String(),
			Platform:       platformRow.Platform,
			PlatformLiveID: platformRow.PlatformLiveID,
			AddedAt:        platformRow.AddedAt.Time,
		}
	}

	// Emit session.created in the same tx (transactional outbox), carrying the
	// assigned sequence_order so consumers see session ordering.
	if err := emitSessionCreated(ctx, qtx, sessionRow, platform, platformLiveID); err != nil {
		return SessionRow{}, nil, err
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return SessionRow{}, nil, fmt.Errorf("committing transaction: %w", err)
	}

	return toSessionRow(sessionRow), platformOut, nil
}

// emitSessionCreated writes the canonical session.created event to the outbox
// within the caller's transaction. Carries the assigned sequence_order (LIV-83)
// so consumers observe session ordering.
func emitSessionCreated(ctx context.Context, q *sqlc.Queries, s sqlc.LiveSession, platform, platformLiveID string) error {
	payload, err := json.Marshal(struct {
		EventID        string `json:"event_id"`
		SessionID      string `json:"session_id"`
		SequenceOrder  int32  `json:"sequence_order"`
		Platform       string `json:"platform"`
		PlatformLiveID string `json:"platform_live_id"`
	}{
		EventID:        s.EventID.String(),
		SessionID:      s.ID.String(),
		SequenceOrder:  s.SequenceOrder,
		Platform:       platform,
		PlatformLiveID: platformLiveID,
	})
	if err != nil {
		return fmt.Errorf("marshaling session.created payload: %w", err)
	}
	return events.Emit(ctx, q, events.Envelope{
		Name:        events.SessionCreated,
		Source:      events.SourceInternal,
		DedupKey:    "session.created:" + s.ID.String(),
		LiveEventID: s.EventID.String(),
		Payload:     payload,
	})
}

// emitEventCreated writes the canonical event.created event to the outbox
// within the caller's transaction.
// sessionType viaja no payload no lugar do antigo live_events.type: o evento
// deixou de ter tipo na 000122, e um payload com "type" derivado seria a mesma
// mentira em outro lugar. O que a primeira sessão é continua sendo informação
// útil para quem lê o log de eventos.
func emitEventCreated(ctx context.Context, q *sqlc.Queries, e sqlc.LiveEvent, sessionType string) error {
	payload, err := json.Marshal(struct {
		EventID     string `json:"event_id"`
		StoreID     string `json:"store_id"`
		SessionType string `json:"session_type"`
		Status      string `json:"status"`
	}{
		EventID:     e.ID.String(),
		StoreID:     e.StoreID.String(),
		SessionType: sessionType,
		Status:      e.Status,
	})
	if err != nil {
		return fmt.Errorf("marshaling event.created payload: %w", err)
	}
	return events.Emit(ctx, q, events.Envelope{
		Name:        events.EventEventCreated,
		Source:      events.SourceInternal,
		DedupKey:    "event.created:" + e.ID.String(),
		LiveEventID: e.ID.String(),
		Payload:     payload,
	})
}

// CreateEventWithSessionTx creates an event, session, and platform in a single transaction.
// This ensures atomicity - either all operations succeed or all are rolled back.
//
// A SESSÃO É CONDICIONAL: só nasce quando a criação TRAZ uma transmissão —
// mídia vinculada (o atalho de post/story, que entra por CreatePostEvent) ou
// uma lista de produtos, que é da sessão e não teria onde morar sem ela.
//
// Uma campanha criada pelo formulário do painel não traz nenhum dos dois: o
// lojista marca a "Semana Black" e pendura as transmissões depois. Criar a
// sessão mesmo assim é o que fazia o painel anunciar "Esta campanha tem uma
// transmissão (Live 1)" para uma campanha recém-criada, vazia — uma transmissão
// que o lojista nunca criou e que aparecia como se estivesse no ar.
//
// Isto JÁ FOI obrigatório, e a razão morreu: a whitelist de EVENTO
// (POST /lives/:id/whitelist) precisava de uma sessão onde gravar, senão
// escrevia zero linhas e lia 404. Essa rota não existe mais — a lista de
// produtos pertence à transmissão e só se configura lá (session_product.sql).
// Evento sem sessão voltou a ser um estado legítimo, e a leitura já o trata:
// o checkout libera tudo quando o evento não tem sessão nenhuma.
func (r *Repository) CreateEventWithSessionTx(ctx context.Context, params CreateEventParams, platform, platformLiveID string) (EventRow, SessionRow, *PlatformRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return EventRow{}, SessionRow{}, nil, err
	}

	// D3: params.Type chega no vocabulário da SESSÃO (live|post|reel|story) ou
	// no legado (single|multi), que os formulários antigos ainda mandam. A
	// SESSÃO é o único lugar que guarda isso desde a 000122 — o evento não tem
	// mais coluna de tipo, porque uma campanha mista não tem resposta única.
	sessionType := SessionTypeFromEventType(params.Type)

	// Convert nullable ints to pgtype.Int4
	var cartExpirationMinutes, cartMaxQuantityPerItem pgtype.Int4
	if params.CartExpirationMinutes != nil {
		cartExpirationMinutes = pgtype.Int4{Int32: int32(*params.CartExpirationMinutes), Valid: true}
	}
	if params.CartMaxQuantityPerItem != nil {
		cartMaxQuantityPerItem = pgtype.Int4{Int32: int32(*params.CartMaxQuantityPerItem), Valid: true}
	}

	// Convert nullable bool to pgtype.Bool
	var autoSendCheckoutLinks pgtype.Bool
	if params.SendOnLiveEnd != nil {
		autoSendCheckoutLinks = pgtype.Bool{Bool: *params.SendOnLiveEnd, Valid: true}
	}

	// starts_at e scheduled_at sao escritos EM PAR, com o mesmo valor, pelo
	// mesmo motivo de SetEventWindow: starts_at e a coluna nova (000114) e
	// scheduled_at e a legada que EffectiveStatus ainda le. Divergir as duas
	// aqui faria o evento nascer com um inicio que a leitura nao enxerga — foi
	// exatamente o que aconteceu quando a janela entrou no INSERT e o par se
	// perdeu.
	var startsAt pgtype.Timestamptz
	if params.StartsAt != nil {
		startsAt = pgtype.Timestamptz{Time: *params.StartsAt, Valid: true}
	}
	// ends_at é NOT NULL desde a 000122. Chegar aqui com nil é bug de chamador,
	// não entrada de usuário: o Service já recusa com 400 antes. Falhar aqui,
	// e não no banco, deixa o erro dizer QUEM esqueceu.
	if params.EndsAt == nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("creating live event: ends_at is required")
	}
	endsAt := pgtype.Timestamptz{Time: *params.EndsAt, Valid: true}
	var description pgtype.Text
	if params.Description != nil {
		description = pgtype.Text{String: *params.Description, Valid: true}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// 1. Create the event
	eventRow, err := qtx.CreateLiveEventFull(ctx, sqlc.CreateLiveEventFullParams{
		StoreID:                storeUID,
		Title:                  pgtype.Text{String: params.Title, Valid: params.Title != ""},
		Status:                 params.Status,
		CloseCartOnEventEnd:    params.CloseCartOnEventEnd,
		CartExpirationMinutes:  cartExpirationMinutes,
		CartMaxQuantityPerItem: cartMaxQuantityPerItem,
		SendOnLiveEnd:          autoSendCheckoutLinks,
		ScheduledAt:            startsAt,
		StartsAt:               startsAt,
		EndsAt:                 endsAt,
		Description:            description,
	})
	if err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("creating live event: %w", err)
	}

	// 2. Create the session — só quando a criação traz transmissão (ver o
	// cabeçalho da função). Campanha vazia fica sem sessão, de propósito.
	hasSession := platformLiveID != "" || len(params.ProductIDs) > 0

	var (
		sessionRow  sqlc.LiveSession
		platformRow *PlatformRow
	)

	if hasSession {
		sessionRow, err = qtx.CreateLiveSession(ctx, sqlc.CreateLiveSessionParams{
			EventID: eventRow.ID,
			Status:  "active",
			Type:    sessionType,
		})
		if err != nil {
			return EventRow{}, SessionRow{}, nil, fmt.Errorf("creating live session: %w", err)
		}

		// 3. A lista de produtos vendáveis da PRIMEIRA transmissão, na MESMA
		// transação em que ela nasce.
		//
		// É o atalho de post/story: o lojista escolhe os produtos no formulário, e
		// esses produtos são desta transmissão — não da campanha, que não tem lista.
		// Antes isso era um laço FORA da transação, chamando a rota por evento
		// (broadcast em todas as sessões) e só logando Warn a cada falha: a
		// publicação já estava no ar e a campanha ficava com lista PARCIAL — ou
		// vazia, que sob a regra "vazia vende tudo" libera o catálogo inteiro,
		// exatamente o oposto do que o lojista pediu. Aqui, ou grava tudo, ou não
		// existe evento.
		for i, productID := range params.ProductIDs {
			productUID, err := parseUUID(productID)
			if err != nil {
				return EventRow{}, SessionRow{}, nil, err
			}
			if _, err := qtx.UpsertSessionProduct(ctx, sqlc.UpsertSessionProductParams{
				SessionID:    sessionRow.ID,
				ProductID:    productUID,
				DisplayOrder: int32(i),
			}); err != nil {
				return EventRow{}, SessionRow{}, nil, fmt.Errorf("adding product %s to the session product list: %w", productID, err)
			}
		}

		// 4. Add the platform to the session — só quando a mídia já é conhecida.
		if platform != "" && platformLiveID != "" {
			row, err := qtx.AddPlatformToSession(ctx, sqlc.AddPlatformToSessionParams{
				SessionID:      sessionRow.ID,
				Platform:       platform,
				PlatformLiveID: platformLiveID,
			})
			if err != nil {
				return EventRow{}, SessionRow{}, nil, fmt.Errorf("adding platform to session: %w", err)
			}
			platformRow = &PlatformRow{
				ID:             row.ID.String(),
				SessionID:      row.SessionID.String(),
				Platform:       row.Platform,
				PlatformLiveID: row.PlatformLiveID,
				AddedAt:        row.AddedAt.Time,
			}
		}
	}

	// Emit event.created + session.created in the same tx (transactional outbox).
	if err := emitEventCreated(ctx, qtx, eventRow, sessionType); err != nil {
		return EventRow{}, SessionRow{}, nil, err
	}
	// session.created só quando houve sessão: emitir para uma sessão que não
	// existe entregaria ao consumidor um id zerado.
	if hasSession {
		if err := emitSessionCreated(ctx, qtx, sessionRow, platform, platformLiveID); err != nil {
			return EventRow{}, SessionRow{}, nil, err
		}
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("committing transaction: %w", err)
	}

	// SessionRow zerada quando não houve sessão. Passar a linha zerada por
	// toSessionRow devolveria o UUID nulo formatado ("0000...0000") como se
	// fosse um id de verdade — o chamador não teria como distinguir.
	if !hasSession {
		return toEventRow(eventRow), SessionRow{}, platformRow, nil
	}
	return toEventRow(eventRow), toSessionRow(sessionRow), platformRow, nil
}

// =============================================================================
// EVENT OPERATIONS
// =============================================================================

// CreateEvent (evento SEM sessão) foi removida: era o único caminho capaz de
// produzir um evento sem sessão nenhuma, e desde que a whitelist (000112) e o
// modo live (000113) desceram para live_sessions esse evento não tem onde
// guardar configuração. Toda criação passa por CreateEventWithSessionTx, que
// aceita mídia vazia.

// GetPixDiscountPercent returns the pix_discount_percent column for an event.
// Loaded via raw SQL because the field is not yet wired through sqlc.
func (r *Repository) GetPixDiscountPercent(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	var pct int32
	err = r.pool.QueryRow(ctx, `SELECT pix_discount_percent FROM live_events WHERE id = $1`, uid).Scan(&pct)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, httpx.ErrNotFound("live event not found")
		}
		return 0, fmt.Errorf("getting pix_discount_percent: %w", err)
	}
	return int(pct), nil
}

// SetPixDiscountPercent updates the pix_discount_percent column for an event.
// Caller validates the percent range (0-100); the table CHECK constraint
// enforces it again at the database boundary.
func (r *Repository) SetPixDiscountPercent(ctx context.Context, eventID, storeID string, percent int) error {
	uid, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE live_events
		SET pix_discount_percent = $3, updated_at = now()
		WHERE id = $1 AND store_id = $2
	`, uid, storeUID, int32(percent))
	if err != nil {
		return fmt.Errorf("setting pix_discount_percent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.ErrNotFound("live event not found")
	}
	return nil
}

// SetMedia grava a legenda, o permalink e a thumbnail DA MÍDIA.
//
// D1/A4: a verdade é a MÍDIA (live_session_platforms), chaveada pelo
// platform_live_id — que é o próprio media_id. O dual-write em live_events
// existiu só para permitir reverter a 000111 sem perder legenda e permalink, e
// saiu junto com as colunas na 000122.
//
// Nome e assinatura mudaram junto (era SetEventMedia, com eventID e storeID):
// os dois argumentos só serviam ao UPDATE do evento. Manter um parâmetro que a
// função ignora é o que faz o próximo leitor achar que a escrita é por evento —
// e um evento guarda-chuva tem N mídias.
func (r *Repository) SetMedia(ctx context.Context, media PostMediaInput) error {
	if media.MediaID == "" {
		return httpx.ErrBadRequest("media id is required")
	}
	if err := r.q.SetMediaMetadata(ctx, sqlc.SetMediaMetadataParams{
		PlatformLiveID:    media.MediaID,
		MediaPermalink:    pgtype.Text{String: media.Permalink, Valid: media.Permalink != ""},
		MediaThumbnailUrl: pgtype.Text{String: media.ThumbnailURL, Valid: media.ThumbnailURL != ""},
		MediaCaption:      pgtype.Text{String: media.Caption, Valid: media.Caption != ""},
	}); err != nil {
		return fmt.Errorf("setting media metadata: %w", err)
	}
	return nil
}

// SetWaitlistNotifiedTTLMinutes grava a janela extra do promovido da fila
// (RN-10). A coluna existe em live_events desde a 000073 com CHECK 5..240 e o
// runtime já a consome (GetWaitlistNotifiedTTL → notifiedUntil → o expires_at
// do carrinho é empurrado com GREATEST); faltava só o caminho de escrita.
// O clamp aqui é o guarda-costas do CHECK: um valor fora da faixa viraria 500.
func (r *Repository) SetWaitlistNotifiedTTLMinutes(ctx context.Context, eventID, storeID string, minutes int) error {
	if minutes < 5 {
		minutes = 5
	}
	if minutes > 240 {
		minutes = 240
	}
	uid, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE live_events
		SET waitlist_notified_ttl_minutes = $3, updated_at = now()
		WHERE id = $1 AND store_id = $2
	`, uid, storeUID, int32(minutes))
	if err != nil {
		return fmt.Errorf("setting waitlist_notified_ttl_minutes: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.ErrNotFound("live event not found")
	}
	return nil
}

// SetEventWindow persiste a janela comercial do evento (D5/D21) de forma
// PARCIAL: cada coluna só é escrita quando o flag correspondente estiver
// ligado. A versão anterior gravava scheduled_at e ends_at juntos e
// incondicionalmente, o que fazia uma edição só do fim apagar o início.
//
// starts_at e scheduled_at são escritos em par: starts_at é a coluna nova
// (000114) e scheduled_at é a legada que EffectiveStatus, o FE e as leituras
// ainda consomem. O contract (000122) NÃO as separou: escrever só starts_at
// deixaria EffectiveStatus cego para o início do evento, então o par continua
// sendo escrito junto — aqui e no INSERT.
func (r *Repository) SetEventWindow(ctx context.Context, eventID, storeID string, w EventWindowUpdate) error {
	if !w.Touches() {
		return nil
	}
	uid, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	var start, end pgtype.Timestamptz
	if w.StartsAt != nil {
		start = pgtype.Timestamptz{Time: *w.StartsAt, Valid: true}
	}
	if w.EndsAt != nil {
		end = pgtype.Timestamptz{Time: *w.EndsAt, Valid: true}
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE live_events
		SET starts_at    = CASE WHEN $3::bool THEN $4::timestamptz ELSE starts_at    END,
		    scheduled_at = CASE WHEN $3::bool THEN $4::timestamptz ELSE scheduled_at END,
		    ends_at      = CASE WHEN $5::bool THEN $6::timestamptz ELSE ends_at      END,
		    updated_at   = now()
		WHERE id = $1 AND store_id = $2
	`, uid, storeUID, w.SetStartsAt, start, w.SetEndsAt, end)
	if err != nil {
		return fmt.Errorf("setting event window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.ErrNotFound("event not found")
	}
	return nil
}

// GetEventMedia e GetActivePostEventByMediaID foram REMOVIDAS aqui: as duas não
// tinham nenhum chamador no repositório inteiro (regra nº 1 — não portar código
// morto para a estrutura nova). Eram, respectivamente, a única leitura das
// colunas media_* do evento e a "query nº 1" que o plano dimensionava como viva.

// ListPollableMedia returns the media that still depend on the polling capture
// loop — post/reel media whose comments webhook has not arrived yet.
//
// D3/A4: passou a iterar sobre MÍDIA, não sobre evento. Antes, um evento com
// duas mídias marcava webhook_active no evento inteiro e a segunda mídia nascia
// com o polling já desligado, sem nunca capturar comentário.
func (r *Repository) ListPollableMedia(ctx context.Context) ([]MediaRef, error) {
	rows, err := r.q.ListPollableMedia(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing pollable media: %w", err)
	}
	out := make([]MediaRef, len(rows))
	for i, row := range rows {
		out[i] = MediaRef{
			MediaID:       row.PlatformLiveID,
			SessionID:     row.SessionID.String(),
			SessionType:   row.SessionType,
			EventID:       row.EventID.String(),
			StoreID:       row.StoreID.String(),
			EventStatus:   row.EventStatus,
			WebhookActive: row.WebhookActive,
		}
	}
	return out, nil
}

// MarkMediaWebhookActive flags that a comments webhook arrived for THIS media,
// so the polling loop stops handling it. Escopo de mídia, não de evento.
func (r *Repository) MarkMediaWebhookActive(ctx context.Context, mediaID string) error {
	return r.q.MarkMediaWebhookActive(ctx, mediaID)
}

// EndEventByMediaID closes the not-yet-ended event that owns mediaID. Used when
// Instagram reports the media is gone (deleted / no longer accessible) so the
// polling loop stops hammering a dead media id every tick.
//
// Resolve pela MÍDIA (live_session_platforms → live_sessions), não mais por
// live_events.media_id.
func (r *Repository) EndEventByMediaID(ctx context.Context, mediaID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE live_events e
		SET status = 'ended', updated_at = now()
		WHERE e.status <> 'ended'
		  AND EXISTS (
		      SELECT 1
		      FROM live_sessions ls
		      JOIN live_session_platforms lsp ON lsp.session_id = ls.id
		      WHERE ls.event_id = e.id
		        AND lsp.platform_live_id = $1
		        AND ls.type IN ('post', 'reel', 'story')
		  )
	`, mediaID)
	return err
}

// TimedEventRef identifies an event for the window sweeps (ready-to-start and
// past-ends_at) and for the media-gone reroute (D5).
type TimedEventRef struct {
	EventID string
	StoreID string
}

// ListEventsReadyToStart returns the scheduled events whose start instant has
// passed and whose window is still open — the ones the activation sweep flips
// to 'active' (E37).
func (r *Repository) ListEventsReadyToStart(ctx context.Context, limit int32) ([]TimedEventRef, error) {
	rows, err := r.q.ListEventsReadyToStart(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events ready to start: %w", err)
	}
	out := make([]TimedEventRef, len(rows))
	for i, row := range rows {
		out[i] = TimedEventRef{EventID: row.ID.String(), StoreID: row.StoreID.String()}
	}
	return out, nil
}

// ActivateScheduledEvent flips a 'scheduled' event to 'active'. Devolve false
// quando nada foi escrito — evento inexistente, já ativo ou já encerrado —, que
// é a resposta esperada e não um erro: o UPDATE é o próprio guard de corrida
// entre o botão do lojista e o sweep.
func (r *Repository) ActivateScheduledEvent(ctx context.Context, eventID string) (bool, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return false, err
	}
	n, err := r.q.ActivateScheduledEvent(ctx, uid)
	if err != nil {
		return false, fmt.Errorf("activating scheduled event: %w", err)
	}
	return n > 0, nil
}

// ListEventsPastEndsAt returns active post/story events whose ends_at window has
// closed — they need End() to finalize their carts (which the read-only
// EffectiveStatus never does).
func (r *Repository) ListEventsPastEndsAt(ctx context.Context, limit int32) ([]TimedEventRef, error) {
	rows, err := r.q.ListEventsPastEndsAt(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events past ends_at: %w", err)
	}
	out := make([]TimedEventRef, len(rows))
	for i, row := range rows {
		out[i] = TimedEventRef{EventID: row.ID.String(), StoreID: row.StoreID.String()}
	}
	return out, nil
}

// GetActiveTimedEventByMediaID resolves the active post/story event mapped to a
// media_id, so the media-gone path can route through End (finalize) instead of a
// bare status flip. Returns nil when there is no such event.
func (r *Repository) GetActiveTimedEventByMediaID(ctx context.Context, mediaID string) (*TimedEventRef, error) {
	row, err := r.q.GetActiveTimedEventByMediaID(ctx, mediaID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolving timed event by media id: %w", err)
	}
	return &TimedEventRef{EventID: row.ID.String(), StoreID: row.StoreID.String()}, nil
}

// GetEventPulse returns a tiny "did anything change" snapshot for an event,
// powering the dashboard's near-real-time refresh. It reads the counters the
// service already maintains (event.total_orders, sessions.total_comments) plus
// the latest cart change — a single cheap, indexed read per poll, so the client
// only refetches the heavy lists when one of these moves.
func (r *Repository) GetEventPulse(ctx context.Context, eventID, storeID string) (EventPulse, error) {
	eid, err := parseUUID(eventID)
	if err != nil {
		return EventPulse{}, err
	}
	sid, err := parseUUID(storeID)
	if err != nil {
		return EventPulse{}, err
	}
	// carts has no updated_at column — the change signal is the newest of:
	// cart creation, payment, and any item mutation (cart_mutations records every
	// quantity/item change with its own created_at).
	var p EventPulse
	err = r.pool.QueryRow(ctx, `
		SELECT
			e.total_orders,
			COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = e.id), 0)::int,
			GREATEST(
				COALESCE((SELECT MAX(c.created_at) FROM carts c WHERE c.event_id = e.id), e.updated_at),
				COALESCE((SELECT MAX(c.paid_at) FROM carts c WHERE c.event_id = e.id), e.updated_at),
				COALESCE((SELECT MAX(cm.created_at)
				          FROM cart_mutations cm
				          JOIN carts c2 ON c2.id = cm.cart_id
				          WHERE c2.event_id = e.id), e.updated_at)
			)
		FROM live_events e
		WHERE e.id = $1 AND e.store_id = $2
	`, eid, sid).Scan(&p.Orders, &p.Comments, &p.OrdersChangedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventPulse{}, httpx.ErrNotFound("event not found")
		}
		return EventPulse{}, fmt.Errorf("getting event pulse: %w", err)
	}
	return p, nil
}

func (r *Repository) GetEventByID(ctx context.Context, id, storeID string) (*EventRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetLiveEventByIDAndStore(ctx, sqlc.GetLiveEventByIDAndStoreParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("live event not found")
		}
		return nil, fmt.Errorf("getting live event: %w", err)
	}

	out := toEventRow(row)
	return &out, nil
}

// GetActiveEventByStore foi REMOVIDA junto da query GetActiveLiveEventByStore.
// Ela respondia "o evento ativo da loja" — uma pergunta que o guarda-chuva
// tornou plural. Com campanhas longas e sobrepostas, um LIMIT 1 por created_at
// DESC devolve a mais recente, que é a resposta errada com a mesma frequência
// com que é a certa. Não tinha chamador; religá-la traria de volta, junto, o
// filtro status = 'active' que a D19/D20 tirou de todas as resoluções
// justamente para o sistema parar de descartar comentário em silêncio.

func (r *Repository) EndEvent(ctx context.Context, id, storeID string) (EventRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return EventRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return EventRow{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventRow{}, fmt.Errorf("begin end-event tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := r.q.WithTx(tx)

	row, err := qtx.EndLiveEvent(ctx, sqlc.EndLiveEventParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("live event not found")
		}
		return EventRow{}, fmt.Errorf("ending live event: %w", err)
	}

	// event.ended in the same tx (transactional outbox).
	payload, err := json.Marshal(struct {
		EventID string `json:"event_id"`
		StoreID string `json:"store_id"`
	}{EventID: row.ID.String(), StoreID: row.StoreID.String()})
	if err != nil {
		return EventRow{}, fmt.Errorf("marshaling event.ended payload: %w", err)
	}
	if err := events.Emit(ctx, qtx, events.Envelope{
		Name:        events.EventEventEnded,
		Source:      events.SourceInternal,
		DedupKey:    "event.ended:" + row.ID.String(),
		LiveEventID: row.ID.String(),
		Payload:     payload,
	}); err != nil {
		return EventRow{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return EventRow{}, fmt.Errorf("commit end-event tx: %w", err)
	}

	return toEventRow(row), nil
}

func (r *Repository) UpdateEventTitle(ctx context.Context, id, title string) (EventRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return EventRow{}, err
	}

	row, err := r.q.UpdateLiveEventTitle(ctx, sqlc.UpdateLiveEventTitleParams{
		ID:    uid,
		Title: pgtype.Text{String: title, Valid: title != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("live event not found")
		}
		return EventRow{}, fmt.Errorf("updating live event title: %w", err)
	}

	return toEventRow(row), nil
}

func (r *Repository) ListEvents(ctx context.Context, storeID string, pagination, offset int) ([]EventRow, int, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.q.ListLiveEventsByStore(ctx, storeUID)
	if err != nil {
		return nil, 0, fmt.Errorf("listing live events: %w", err)
	}

	events := make([]EventRow, len(rows))
	for i, row := range rows {
		events[i] = toEventRow(row)
	}

	return events, len(events), nil
}

func (r *Repository) GetEventByPlatformLiveID(ctx context.Context, platformLiveID string) (*EventRow, error) {
	row, err := r.q.GetEventByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting event by platform live id: %w", err)
	}

	out := toEventRow(row)
	return &out, nil
}

func (r *Repository) CountSessionsByEvent(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	count, err := r.q.CountSessionsByEvent(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting sessions: %w", err)
	}

	return int(count), nil
}

func (r *Repository) DeleteEvent(ctx context.Context, id, storeID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	result, err := r.pool.Exec(ctx, "DELETE FROM live_events WHERE id = $1 AND store_id = $2", uid, storeUID)
	if err != nil {
		return fmt.Errorf("deleting live event: %w", err)
	}

	if result.RowsAffected() == 0 {
		return httpx.ErrNotFound("live event not found")
	}

	return nil
}

// =============================================================================
// SESSION OPERATIONS
// =============================================================================

// Repository.CreateSession foi REMOVIDA. Era um SEGUNDO caminho de criação de
// sessão: chamava CreateLiveSession direto, fora de transação, sem vincular
// mídia e sem emitir session.created no outbox. Duas portas para o mesmo ato é
// como cada regra nova acaba escrita só numa delas.
//
// Não tinha chamador. O ponto único de criação é CreateSessionWithPlatformTx,
// que cria sessão + plataforma + evento de outbox na MESMA transação.

func (r *Repository) GetSessionByID(ctx context.Context, id string) (*SessionRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetLiveSessionByID(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("live session not found")
		}
		return nil, fmt.Errorf("getting live session: %w", err)
	}

	out := toSessionRow(row)
	return &out, nil
}

func (r *Repository) GetActiveSessionByEvent(ctx context.Context, eventID string) (*SessionRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetActiveSessionByEvent(ctx, eventUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting active session: %w", err)
	}

	out := toSessionRow(row)
	return &out, nil
}

func (r *Repository) StartSession(ctx context.Context, id string) (SessionRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return SessionRow{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SessionRow{}, fmt.Errorf("begin start-session tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := r.q.WithTx(tx)

	row, err := qtx.StartLiveSession(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRow{}, httpx.ErrNotFound("live session not found")
		}
		return SessionRow{}, fmt.Errorf("starting live session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return SessionRow{}, fmt.Errorf("commit start-session tx: %w", err)
	}

	return toSessionRow(row), nil
}

func (r *Repository) EndSession(ctx context.Context, id string) (SessionRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return SessionRow{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SessionRow{}, fmt.Errorf("begin end-session tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := r.q.WithTx(tx)

	row, err := qtx.EndLiveSession(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRow{}, httpx.ErrNotFound("live session not found")
		}
		return SessionRow{}, fmt.Errorf("ending live session: %w", err)
	}

	// session.ended in the same tx (transactional outbox).
	payload, err := json.Marshal(struct {
		EventID       string `json:"event_id"`
		SessionID     string `json:"session_id"`
		SequenceOrder int32  `json:"sequence_order"`
	}{EventID: row.EventID.String(), SessionID: row.ID.String(), SequenceOrder: row.SequenceOrder})
	if err != nil {
		return SessionRow{}, fmt.Errorf("marshaling session.ended payload: %w", err)
	}
	if err := events.Emit(ctx, qtx, events.Envelope{
		Name:        events.SessionEnded,
		Source:      events.SourceInternal,
		DedupKey:    "session.ended:" + row.ID.String(),
		LiveEventID: row.EventID.String(),
		Payload:     payload,
	}); err != nil {
		return SessionRow{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return SessionRow{}, fmt.Errorf("commit end-session tx: %w", err)
	}

	return toSessionRow(row), nil
}

func (r *Repository) ListSessionsByEvent(ctx context.Context, eventID string) ([]SessionRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListSessionsByEvent(ctx, eventUID)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}

	sessions := make([]SessionRow, len(rows))
	for i, row := range rows {
		sessions[i] = toSessionRow(row)
	}

	return sessions, nil
}

func (r *Repository) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*SessionRow, error) {
	row, err := r.q.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting session by platform live id: %w", err)
	}

	out := toSessionRow(row)
	return &out, nil
}

// SetSessionPublishAt grava a hora em que a transmissão foi publicada por
// agendamento (RN-31). Ver MarkSessionPublished no service.
func (r *Repository) SetSessionPublishAt(ctx context.Context, sessionID string, publishAt time.Time) error {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return err
	}
	return r.q.SetSessionPublishAt(ctx, sqlc.SetSessionPublishAtParams{
		ID:        uid,
		PublishAt: pgtype.Timestamptz{Time: publishAt, Valid: true},
	})
}

func (r *Repository) IncrementSessionComments(ctx context.Context, sessionID string) error {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return err
	}

	return r.q.IncrementLiveSessionComments(ctx, uid)
}

// ReplyTarget é o comentário que receberá a resposta privada, com a idade dele.
//
// A idade viaja junto de propósito: quem decide se ainda dá para responder é o
// domínio da notificação (7 dias de private reply), e é ele que precisa
// distinguir "não há comentário nenhum" de "há, mas venceu" — os dois motivos
// que o lojista vê na lista da RN-38 e que exigem ações diferentes dele.
type ReplyTarget struct {
	CommentID string
	CreatedAt *time.Time
}

// GetLatestReplyTarget returns the most recent USABLE Instagram comment a buyer
// posted in an event — one that hasn't consumed its single private reply and
// isn't hidden/deleted (Instagram rejects replies to those, error 2534066).
// Returns an empty target (no error) when the buyer has no usable comment.
//
// NÃO filtra por idade. O filtro `created_at >= now() - interval '7 days'`
// morava aqui e escondia a informação de que o chamador precisa: com ele, um
// comprador cujo único comentário venceu era indistinguível de um comprador que
// nunca comentou — os dois davam "". A janela continua sendo respeitada, agora
// no ponto que também REGISTRA a não entrega com o motivo certo, em vez de
// degradar em silêncio para um DM que o Instagram recusa.
func (r *Repository) GetLatestReplyTarget(ctx context.Context, eventID, platformUserID string) (ReplyTarget, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return ReplyTarget{}, err
	}

	var commentID string
	var createdAt time.Time
	err = r.pool.QueryRow(ctx, `
		SELECT platform_comment_id, created_at FROM live_comments
		WHERE event_id = $1 AND platform_user_id = $2 AND platform_comment_id <> ''
		  AND NOT private_reply_used AND NOT hidden AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`, eventUID, platformUserID).Scan(&commentID, &createdAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReplyTarget{}, nil
		}
		return ReplyTarget{}, fmt.Errorf("getting latest reply target: %w", err)
	}
	return ReplyTarget{CommentID: commentID, CreatedAt: &createdAt}, nil
}

// ListCommentsBySession returns all comments for a session.
func (r *Repository) ListCommentsBySession(ctx context.Context, sessionID string, limit, offset int) ([]CommentRow, error) {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListCommentsBySession(ctx, sqlc.ListCommentsBySessionParams{
		SessionID: uid,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("listing comments by session: %w", err)
	}

	comments := make([]CommentRow, 0, len(rows))
	for _, row := range rows {
		comments = append(comments, CommentRow{
			ID:                row.ID.String(),
			SessionID:         row.SessionID.String(),
			PlatformCommentID: row.PlatformCommentID,
			PlatformUserID:    row.PlatformUserID,
			PlatformHandle:    row.PlatformHandle,
			Text:              row.Text,
			HasPurchaseIntent: row.HasPurchaseIntent.Bool,
			CreatedAt:         row.CreatedAt.Time,
		})
	}

	return comments, nil
}

// ListCommentsByEvent returns comments for an event, including the Instagram
// comment ID needed for moderation (reply / hide / delete) and the mirrored
// hidden state so the UI's hide button can toggle (hide ↔ unhide).
func (r *Repository) ListCommentsByEvent(ctx context.Context, eventID string, limit, offset int) ([]CommentRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, session_id, platform_comment_id, platform_user_id, platform_handle,
		       text, COALESCE(has_purchase_intent, false), hidden, created_at
		FROM live_comments
		WHERE event_id = $1 AND deleted_at IS NULL
		ORDER BY created_at
		LIMIT $2 OFFSET $3
	`, uid, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("listing comments by event: %w", err)
	}
	defer rows.Close()

	comments := make([]CommentRow, 0)
	for rows.Next() {
		var c CommentRow
		var id, sessionID pgtype.UUID
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(&id, &sessionID, &c.PlatformCommentID, &c.PlatformUserID,
			&c.PlatformHandle, &c.Text, &c.HasPurchaseIntent, &c.Hidden, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning comment row: %w", err)
		}
		c.ID = id.String()
		c.SessionID = sessionID.String()
		c.CreatedAt = createdAt.Time
		comments = append(comments, c)
	}

	return comments, rows.Err()
}

// =============================================================================
// PLATFORM OPERATIONS
// =============================================================================

func (r *Repository) AddPlatformToSession(ctx context.Context, sessionID, platform, platformLiveID string) (*PlatformRow, error) {
	sID, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.AddPlatformToSession(ctx, sqlc.AddPlatformToSessionParams{
		SessionID:      sID,
		Platform:       platform,
		PlatformLiveID: platformLiveID,
	})
	if err != nil {
		return nil, fmt.Errorf("adding platform to session: %w", err)
	}

	return &PlatformRow{
		ID:             row.ID.String(),
		SessionID:      row.SessionID.String(),
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt.Time,
	}, nil
}

func (r *Repository) ListPlatformsBySession(ctx context.Context, sessionID string) ([]PlatformRow, error) {
	sID, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPlatformsBySession(ctx, sID)
	if err != nil {
		return nil, fmt.Errorf("listing platforms: %w", err)
	}

	platforms := make([]PlatformRow, len(rows))
	for i, row := range rows {
		platforms[i] = PlatformRow{
			ID:             row.ID.String(),
			SessionID:      row.SessionID.String(),
			Platform:       row.Platform,
			PlatformLiveID: row.PlatformLiveID,
			AddedAt:        row.AddedAt.Time,
		}
	}

	return platforms, nil
}

func (r *Repository) RemovePlatformFromSession(ctx context.Context, sessionID, platformLiveID string) error {
	sID, err := parseUUID(sessionID)
	if err != nil {
		return err
	}

	return r.q.RemovePlatformFromSession(ctx, sqlc.RemovePlatformFromSessionParams{
		SessionID:      sID,
		PlatformLiveID: platformLiveID,
	})
}

// GetPlatformByLiveID foi removida junto com a query: sem chamador e, depois da
// 000117, um `:one` sobre uma coluna que deixou de ser única — devolveria uma
// campanha arbitrária, em silêncio. Ver a nota em db/queries/live.sql.

// =============================================================================
// STORE SETTINGS
// =============================================================================

func (r *Repository) GetStoreAutoSendSetting(ctx context.Context, storeID string) (bool, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return false, err
	}

	store, err := r.q.GetStoreByID(ctx, uid)
	if err != nil {
		return false, fmt.Errorf("getting store: %w", err)
	}

	return store.SendOnLiveEnd, nil
}

// =============================================================================
// CART OPERATIONS (now use event_id)
// =============================================================================

func (r *Repository) GetOrCreateCart(ctx context.Context, params GetOrCreateCartParams) (*CartRow, bool, error) {
	eventID, err := parseUUID(params.EventID)
	if err != nil {
		return nil, false, err
	}

	// Parse session ID if provided (before transaction)
	var sessionID pgtype.UUID
	if params.SessionID != nil {
		sid, err := parseUUID(*params.SessionID)
		if err != nil {
			return nil, false, fmt.Errorf("parsing session ID: %w", err)
		}
		sessionID = sid
	}

	// Parse customer ID if provided
	var customerID pgtype.UUID
	if params.CustomerID != nil {
		cid, err := parseUUID(*params.CustomerID)
		if err != nil {
			return nil, false, fmt.Errorf("parsing customer ID: %w", err)
		}
		customerID = cid
	}

	// Use transaction to ensure atomicity (SELECT + INSERT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// Busca o carrinho ABERTO do comprador neste evento. FOR UPDATE trava a row
	// e resolve a corrida de dois comentários simultâneos do mesmo comprador.
	//
	// Desde a 000107 a query filtra por carrinho aberto, então ela só devolve
	// linha quando existe um carrinho que ainda pode receber item e ser pago.
	// Carrinho pago, expirado, cancelado ou estornado cai em ErrNoRows e o
	// caminho abaixo cria um NOVO — que o índice parcial agora permite.
	//
	// Foi isto que acabou com o reopen destrutivo. Antes, a unique TOTAL impedia
	// um 2º carrinho, então um cart morto era reaberto in-place: os itens do
	// comprador eram APAGADOS (DeleteCartItemsByCart) para o carrinho poder ser
	// reusado. Numa campanha de uma semana isso apagaria a compra de dias
	// (RN-08). Agora o antigo fica arquivado, intacto, e o novo nasce limpo.
	existing, err := qtx.GetCartByEventAndUserForUpdate(ctx, sqlc.GetCartByEventAndUserForUpdateParams{
		EventID:        eventID,
		PlatformUserID: params.PlatformUserID,
	})
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("committing transaction: %w", err)
		}
		return &CartRow{
			ID:             existing.ID.String(),
			EventID:        existing.EventID.String(),
			PlatformUserID: existing.PlatformUserID,
			PlatformHandle: existing.PlatformHandle,
			Token:          existing.Token,
		}, false, nil
	}

	// Cart doesn't exist, create a new one
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("getting cart: %w", err)
	}

	// Issue the next human-readable order number for this store. The query
	// resolves store_id from event_id and bumps store_order_counters atomically
	// so concurrent cart creations cannot collide on the same short_id.
	shortID, err := qtx.IssueShortIDForEvent(ctx, eventID)
	if err != nil {
		return nil, false, fmt.Errorf("issuing short id: %w", err)
	}

	// Note: expires_at is NOT set on creation. It will be set when the live event ends.
	created, err := qtx.CreateCart(ctx, sqlc.CreateCartParams{
		EventID:        eventID,
		SessionID:      sessionID,
		PlatformUserID: params.PlatformUserID,
		PlatformHandle: params.PlatformHandle,
		Token:          params.Token,
		CustomerID:     customerID,
		ShortID:        shortID,
	})
	if err != nil {
		return nil, false, fmt.Errorf("creating cart: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("committing transaction: %w", err)
	}

	return &CartRow{
		ID:             created.ID.String(),
		EventID:        created.EventID.String(),
		PlatformUserID: created.PlatformUserID,
		PlatformHandle: created.PlatformHandle,
		Token:          created.Token,
	}, true, nil
}

// emitCartEvent writes a canonical cart lifecycle event to the outbox within
// the caller's transaction. dedupKey may be empty for events that legitimately
// repeat for the same cart.
func emitCartEvent(ctx context.Context, q *sqlc.Queries, name events.Name, cartID, eventID, sessionID, dedupKey string) error {
	payload, err := json.Marshal(struct {
		CartID    string `json:"cart_id"`
		EventID   string `json:"event_id"`
		SessionID string `json:"session_id,omitempty"`
	}{CartID: cartID, EventID: eventID, SessionID: sessionID})
	if err != nil {
		return fmt.Errorf("marshaling %s payload: %w", name, err)
	}
	return events.Emit(ctx, q, events.Envelope{
		Name:        name,
		Source:      events.SourceInternal,
		DedupKey:    dedupKey,
		LiveEventID: eventID,
		Payload:     payload,
	})
}

// EmitPostWindowClosed emits post.window_closed in its own transaction. Called
// by the timed-event sweep / media-deleted path just before Service.End; those
// queries only return still-active events, so it fires once per window close.
func (r *Repository) EmitPostWindowClosed(ctx context.Context, eventID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin post-window tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := r.q.WithTx(tx)

	payload, err := json.Marshal(struct {
		EventID string `json:"event_id"`
	}{EventID: eventID})
	if err != nil {
		return fmt.Errorf("marshaling post.window_closed payload: %w", err)
	}
	if err := events.Emit(ctx, qtx, events.Envelope{
		Name:     events.PostWindowClosed,
		Source:   events.SourceInternal,
		DedupKey: "post.window_closed:" + eventID,
		Payload:  payload,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// pgUUIDString returns the string form of a pgtype.UUID, or "" when unset.
func pgUUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

func (r *Repository) FinalizeCartsByEvent(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	// Use transaction to ensure atomicity (COUNT + UPDATE)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// RN-34: o prazo vem da fonte única (GetEventCartSettings), que já escolhe
	// entre o curto e o estendido conforme close_cart_on_event_end e já faz o
	// fallback para a loja. Antes o COALESCE estava inline no UPDATE e o toggle
	// não era lido por regra nenhuma — o lojista via a opção na tela e ela não
	// fazia nada.
	settings, err := qtx.GetEventCartSettings(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("resolving cart deadline for event: %w", err)
	}

	// Finalize (active carts → checkout). Returns the finalized cart ids so we
	// can emit cart.checkout_armed per cart in the same tx.
	// O extra de quem está na fila vem do EVENTO: é a mesma configuração que o
	// lojista já preenche em "quanto tempo a mais quem espera tem". Antes ela
	// só valia DEPOIS da promoção — e a promoção nunca acontecia, porque quem
	// esperava vencia junto com quem segurava o estoque.
	ids, err := qtx.FinalizeCartsByEvent(ctx, sqlc.FinalizeCartsByEventParams{
		EventID:              uid,
		ExpirationMinutes:    settings.EffectiveCartExpirationMinutes,
		WaitlistExtraMinutes: settings.WaitlistNotifiedTtlMinutes,
	})
	if err != nil {
		return 0, fmt.Errorf("finalizing carts: %w", err)
	}

	for _, id := range ids {
		if err := emitCartEvent(ctx, qtx, events.CartCheckoutArmed, id.String(), eventID, "", "cart.checkout_armed:"+id.String()); err != nil {
			return 0, err
		}
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return len(ids), nil
}

func (r *Repository) AddCartItem(ctx context.Context, params AddCartItemParams) error {
	cartID, err := parseUUID(params.CartID)
	if err != nil {
		return err
	}
	productID, err := parseUUID(params.ProductID)
	if err != nil {
		return err
	}

	var sessionID pgtype.UUID
	if params.SessionID != "" {
		sessionID, err = parseUUID(params.SessionID)
		if err != nil {
			return fmt.Errorf("parsing session id: %w", err)
		}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin add-item tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := r.q.WithTx(tx)

	if _, err = qtx.UpsertCartItem(ctx, sqlc.UpsertCartItemParams{
		CartID:             cartID,
		ProductID:          productID,
		Quantity:           pgtype.Int4{Int32: int32(params.Quantity), Valid: true},
		UnitPrice:          pgtype.Int8{Int64: params.UnitPrice, Valid: true},
		WaitlistedQuantity: int32(params.WaitlistedQuantity),
		SessionID:          sessionID,
	}); err != nil {
		return fmt.Errorf("upserting cart item: %w", err)
	}

	// Log de atribuição (RN-12), no MESMO tx do upsert. cart_items soma a
	// quantidade e guarda só a PRIMEIRA sessão (COALESCE), então sozinho ele
	// credita à live de segunda uma unidade comprada na de quarta. O log guarda
	// cada adição com a sessão que a gerou e o preço praticado na hora.
	if err := qtx.InsertCartItemEvent(ctx, sqlc.InsertCartItemEventParams{
		CartID:    cartID,
		ProductID: productID,
		SessionID: sessionID,
		Quantity:  int32(params.Quantity),
		UnitPrice: params.UnitPrice,
	}); err != nil {
		return fmt.Errorf("logging cart item addition: %w", err)
	}

	// cart.item_added in the same tx (can repeat per cart+product, so no dedup key).
	payload, err := json.Marshal(struct {
		CartID    string `json:"cart_id"`
		ProductID string `json:"product_id"`
		Quantity  int    `json:"quantity"`
		SessionID string `json:"session_id,omitempty"`
	}{CartID: params.CartID, ProductID: params.ProductID, Quantity: params.Quantity, SessionID: params.SessionID})
	if err != nil {
		return fmt.Errorf("marshaling cart.item_added payload: %w", err)
	}
	if err := events.Emit(ctx, qtx, events.Envelope{
		Name:    events.CartItemAdded,
		Source:  events.SourceInternal,
		Payload: payload,
	}); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// GetCartTotals returns total items and value for a cart.
func (r *Repository) GetCartTotals(ctx context.Context, cartID string) (int, int64, error) {
	uid, err := parseUUID(cartID)
	if err != nil {
		return 0, 0, err
	}

	row, err := r.q.GetCartTotals(ctx, uid)
	if err != nil {
		return 0, 0, fmt.Errorf("getting cart totals: %w", err)
	}

	return int(row.TotalItems), row.TotalValue, nil
}

// =============================================================================
// STATS (now from events)
// =============================================================================

func (r *Repository) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return LiveStatsOutput{}, err
	}

	// total_revenue espelha dashboard.Repository.GetStats — e agora espelha de
	// verdade. O comentário antigo prometia que as duas superfícies estavam em
	// sincronia, mas a do dashboard migrou para orders (RN-14, Grupo B) e esta
	// ficou somando TODO cart_item de TODO carrinho da loja, sem filtro de
	// pagamento nenhum: carrinho aberto, expirado e cancelado entravam no
	// "Faturamento" de /events. O lojista via, para a mesma loja, um número
	// maior aqui do que no dashboard — e um comentário afirmando que eram o
	// mesmo número.
	//
	// A subquery abaixo é literalmente a de dashboard.sql:GetDashboardStats.
	query := `
		SELECT
			COUNT(*) as total_lives,
			COUNT(*) FILTER (WHERE status = 'active') as active_lives,
			COALESCE(SUM(total_orders), 0) as total_orders,
			COALESCE((
				SELECT SUM(o.total_cents)
				FROM orders o
				WHERE o.store_id = $1 AND o.status = 'paid'
			), 0)::BIGINT as total_revenue
		FROM live_events
		WHERE store_id = $1
	`

	var stats LiveStatsOutput
	err = r.pool.QueryRow(ctx, query, storeUID).Scan(
		&stats.TotalLives,
		&stats.ActiveLives,
		&stats.TotalOrders,
		&stats.TotalRevenue,
	)
	if err != nil {
		return LiveStatsOutput{}, fmt.Errorf("getting live stats: %w", err)
	}

	return stats, nil
}

// =============================================================================
// LEGACY LIST (joins events with sessions and platforms)
// =============================================================================

type ListLivesParams struct {
	StoreID    string
	Search     string
	Pagination struct {
		Limit  int
		Offset int
	}
	Sorting struct {
		SortBy    string
		SortOrder string
	}
	Filters LiveFilters
}

func (r *Repository) ListLives(ctx context.Context, params ListLivesParams) ([]LiveOutput, int, error) {
	// Build WHERE conditions
	conditions := []string{"e.store_id = $1"}
	args := []interface{}{params.StoreID}
	argIdx := 2

	// Search filter (title)
	if params.Search != "" {
		conditions = append(conditions, fmt.Sprintf("LOWER(e.title) LIKE $%d", argIdx))
		args = append(args, "%"+strings.ToLower(params.Search)+"%")
		argIdx++
	}

	// Status filter
	if len(params.Filters.Status) > 0 {
		placeholders := make([]string, len(params.Filters.Status))
		for i, status := range params.Filters.Status {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, status)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("e.status IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Date filters
	if params.Filters.DateFrom != nil && *params.Filters.DateFrom != "" {
		conditions = append(conditions, fmt.Sprintf("e.created_at >= $%d", argIdx))
		args = append(args, *params.Filters.DateFrom)
		argIdx++
	}
	if params.Filters.DateTo != nil && *params.Filters.DateTo != "" {
		conditions = append(conditions, fmt.Sprintf("e.created_at <= $%d", argIdx))
		args = append(args, *params.Filters.DateTo)
		argIdx++
	}

	whereClause := strings.Join(conditions, " AND ")

	// Validate and build ORDER BY
	allowedSortFields := map[string]string{
		"title":      "e.title",
		"status":     "e.status",
		"created_at": "e.created_at",
	}
	sortField, ok := allowedSortFields[params.Sorting.SortBy]
	if !ok {
		sortField = "e.created_at"
	}
	sortOrder := "DESC"
	if strings.ToUpper(params.Sorting.SortOrder) == "ASC" {
		sortOrder = "ASC"
	}
	orderClause := fmt.Sprintf("%s %s", sortField, sortOrder)

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM live_events e WHERE %s", whereClause)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting live events: %w", err)
	}

	// Build main query with pagination
	// Join with sessions to get the first session's start/end times
	// Join with platforms to get the primary platform
	query := fmt.Sprintf(`
		SELECT
			e.id, e.store_id, e.title, e.status, e.total_orders, e.created_at, e.updated_at,
			e.close_cart_on_event_end, e.cart_expiration_minutes, e.cart_max_quantity_per_item, e.send_on_live_end,
			COALESCE(e.pix_discount_percent, 0),
			e.scheduled_at, e.ends_at,
			s.started_at, s.ended_at, COALESCE(s.total_comments, 0),
			COALESCE(p.platform, ''), COALESCE(p.platform_live_id, ''),
			COALESCE(st.types, ARRAY[]::text[])
		FROM live_events e
		LEFT JOIN LATERAL (
			-- Tipos DISTINTOS das sessões: é o que substitui live_events.type
			-- desde que a 000122 o dropou. Um LATERAL próprio (e não um join na
			-- lateral da primeira sessão) porque a campanha é MISTA: a
			-- primeira sessão pode ser a live e a terceira o story, e rotular
			-- o evento pela primeira sessão seria trocar uma mentira por outra.
			SELECT array_agg(DISTINCT type ORDER BY type) AS types
			FROM live_sessions
			WHERE event_id = e.id
		) st ON true
		LEFT JOIN LATERAL (
			SELECT id, started_at, ended_at, total_comments
			FROM live_sessions
			WHERE event_id = e.id
			ORDER BY created_at ASC
			LIMIT 1
		) s ON true
		LEFT JOIN LATERAL (
			SELECT platform, platform_live_id
			FROM live_session_platforms
			WHERE session_id = s.id
			ORDER BY added_at ASC
			LIMIT 1
		) p ON true
		WHERE %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d
	`, whereClause, orderClause, argIdx, argIdx+1)

	args = append(args, params.Pagination.Limit, params.Pagination.Offset)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing live events: %w", err)
	}
	defer rows.Close()

	lives := make([]LiveOutput, 0)
	for rows.Next() {
		var live LiveOutput
		var title, platform, platformLiveID pgtype.Text
		var startedAt, endedAt, scheduledAt, endsAt pgtype.Timestamptz
		var cartExpirationMinutes, cartMaxQuantityPerItem pgtype.Int4
		var autoSendCheckoutLinks pgtype.Bool
		var pixDiscountPercent int32

		if err := rows.Scan(
			&live.ID,
			&live.StoreID,
			&title,
			&live.Status,
			&live.TotalOrders,
			&live.CreatedAt,
			&live.UpdatedAt,
			&live.CloseCartOnEventEnd,
			&cartExpirationMinutes,
			&cartMaxQuantityPerItem,
			&autoSendCheckoutLinks,
			&pixDiscountPercent,
			&scheduledAt,
			&endsAt,
			&startedAt,
			&endedAt,
			&live.TotalComments,
			&platform,
			&platformLiveID,
			&live.SessionTypes,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning live event: %w", err)
		}
		live.PixDiscountPercent = int(pixDiscountPercent)

		if title.Valid {
			live.Title = title.String
		}
		if platform.Valid {
			live.Platform = platform.String
		}
		if platformLiveID.Valid {
			live.PlatformLiveID = platformLiveID.String
		}
		if startedAt.Valid {
			live.StartedAt = &startedAt.Time
		}
		if endedAt.Valid {
			live.EndedAt = &endedAt.Time
		}
		if cartExpirationMinutes.Valid {
			v := int(cartExpirationMinutes.Int32)
			live.CartExpirationMinutes = &v
		}
		if cartMaxQuantityPerItem.Valid {
			v := int(cartMaxQuantityPerItem.Int32)
			live.CartMaxQuantityPerItem = &v
		}
		if autoSendCheckoutLinks.Valid {
			live.SendOnLiveEnd = &autoSendCheckoutLinks.Bool
		}
		if scheduledAt.Valid {
			live.ScheduledAt = &scheduledAt.Time
		}
		if endsAt.Valid {
			live.EndsAt = &endsAt.Time
		}
		// Show the status derived from the scheduled window (no background job).
		live.Status = EffectiveStatus(live.Status, live.ScheduledAt, live.EndsAt)

		lives = append(lives, live)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating live events: %w", err)
	}

	return lives, total, nil
}

// =============================================================================
// HELPERS
// =============================================================================

func toEventRow(row sqlc.LiveEvent) EventRow {
	var title string
	if row.Title.Valid {
		title = row.Title.String
	}

	// Convert nullable fields
	var cartExpirationMinutes, cartMaxQuantityPerItem *int
	if row.CartExpirationMinutes.Valid {
		v := int(row.CartExpirationMinutes.Int32)
		cartExpirationMinutes = &v
	}
	if row.CartMaxQuantityPerItem.Valid {
		v := int(row.CartMaxQuantityPerItem.Int32)
		cartMaxQuantityPerItem = &v
	}
	var autoSendCheckoutLinks *bool
	if row.SendOnLiveEnd.Valid {
		autoSendCheckoutLinks = &row.SendOnLiveEnd.Bool
	}
	// Scheduling fields
	var scheduledAt *time.Time
	if row.ScheduledAt.Valid {
		scheduledAt = &row.ScheduledAt.Time
	}
	var endsAt *time.Time
	if row.EndsAt.Valid {
		endsAt = &row.EndsAt.Time
	}
	var description *string
	if row.Description.Valid {
		description = &row.Description.String
	}

	return EventRow{
		ID:                     row.ID.String(),
		StoreID:                row.StoreID.String(),
		Title:                  title,
		Status:                 row.Status,
		TotalOrders:            int(row.TotalOrders),
		CloseCartOnEventEnd:    row.CloseCartOnEventEnd,
		CartExpirationMinutes:  cartExpirationMinutes,
		CartMaxQuantityPerItem: cartMaxQuantityPerItem,
		SendOnLiveEnd:          autoSendCheckoutLinks,
		ScheduledAt:            scheduledAt,
		EndsAt:                 endsAt,
		Description:            description,
		CreatedAt:              row.CreatedAt.Time,
		UpdatedAt:              row.UpdatedAt.Time,

		WaitlistNotifiedTTLMinutes: int(row.WaitlistNotifiedTtlMinutes),
	}
}

func toSessionRow(row sqlc.LiveSession) SessionRow {
	var startedAt, endedAt *time.Time
	if row.StartedAt.Valid {
		startedAt = &row.StartedAt.Time
	}
	if row.EndedAt.Valid {
		endedAt = &row.EndedAt.Time
	}

	var activeProductID *string
	if row.CurrentActiveProductID.Valid {
		v := row.CurrentActiveProductID.String()
		activeProductID = &v
	}

	return SessionRow{
		ID:                     row.ID.String(),
		EventID:                row.EventID.String(),
		Type:                   row.Type,
		Status:                 row.Status,
		SequenceOrder:          int(row.SequenceOrder),
		CurrentActiveProductID: activeProductID,
		ProcessingPaused:       row.ProcessingPaused,
		AttributionSource:      row.AttributionSource,
		StartedAt:              startedAt,
		EndedAt:                endedAt,
		TotalComments:          int(row.TotalComments.Int32),
		CreatedAt:              row.CreatedAt.Time,
		UpdatedAt:              row.UpdatedAt.Time,
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing
// =============================================================================

func (r *Repository) GetEventStats(ctx context.Context, eventID string) (EventStatsRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return EventStatsRow{}, err
	}

	row, err := r.q.GetEventStats(ctx, uid)
	if err != nil {
		return EventStatsRow{}, fmt.Errorf("getting event stats: %w", err)
	}

	return EventStatsRow{
		TotalComments:     int(row.TotalComments),
		TotalCarts:        int(row.TotalCarts),
		OpenCarts:         int(row.OpenCarts),
		CheckoutCarts:     int(row.CheckoutCarts),
		PaidCarts:         int(row.PaidCarts),
		TotalProductsSold: int(row.TotalProductsSold),
		ProjectedRevenue:  row.ProjectedRevenue,
		ConfirmedRevenue:  row.ConfirmedRevenue,
	}, nil
}

// ListActiveCheckoutsByEvent returns carts the buyer is currently editing/paying.
// Powers the merchant's live "active checkouts" panel.
func (r *Repository) ListActiveCheckoutsByEvent(ctx context.Context, eventID string) ([]ActiveCheckoutRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListActiveCheckoutsByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing active checkouts: %w", err)
	}
	out := make([]ActiveCheckoutRow, len(rows))
	for i, row := range rows {
		var paymentStatus string
		if row.PaymentStatus.Valid {
			paymentStatus = row.PaymentStatus.String
		}
		var expiresAt *time.Time
		if row.ExpiresAt.Valid {
			expiresAt = &row.ExpiresAt.Time
		}
		var lastMutAt *time.Time
		if row.LastMutationAt.Valid {
			lastMutAt = &row.LastMutationAt.Time
		}
		out[i] = ActiveCheckoutRow{
			ID:                   row.ID.String(),
			PlatformHandle:       row.PlatformHandle,
			Token:                row.Token,
			Status:               row.Status,
			PaymentStatus:        paymentStatus,
			CreatedAt:            row.CreatedAt.Time,
			ExpiresAt:            expiresAt,
			InitialSubtotalCents: row.InitialSubtotalCents,
			CurrentSubtotalCents: row.CurrentSubtotalCents,
			MutationCount:        int(row.MutationCount),
			LastMutationAt:       lastMutAt,
		}
	}
	return out, nil
}

func (r *Repository) ListCartsWithTotalByEvent(ctx context.Context, eventID string) ([]CartWithTotalRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListCartsWithTotalByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing carts with total: %w", err)
	}

	carts := make([]CartWithTotalRow, len(rows))
	for i, row := range rows {
		var sessionID *string
		if row.SessionID.Valid {
			s := row.SessionID.String()
			sessionID = &s
		}
		var paymentStatus *string
		if row.PaymentStatus.Valid {
			paymentStatus = &row.PaymentStatus.String
		}
		var expiresAt *time.Time
		if row.ExpiresAt.Valid {
			expiresAt = &row.ExpiresAt.Time
		}

		carts[i] = CartWithTotalRow{
			ID:              row.ID.String(),
			EventID:         row.EventID.String(),
			SessionID:       sessionID,
			PlatformUserID:  row.PlatformUserID,
			PlatformHandle:  row.PlatformHandle,
			Token:           row.Token,
			Status:          row.Status,
			PaymentStatus:   paymentStatus,
			TotalValue:      row.TotalValue,
			TotalItems:      int(row.TotalItems),
			AvailableItems:  int(row.AvailableItems),
			WaitlistedItems: int(row.WaitlistedItems),
			CreatedAt:       row.CreatedAt.Time,
			ExpiresAt:       expiresAt,
		}
	}

	return carts, nil
}

func (r *Repository) ListProductsByEvent(ctx context.Context, eventID string) ([]EventProductRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListProductsByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing products by event: %w", err)
	}

	products := make([]EventProductRow, len(rows))
	for i, row := range rows {
		var imageURL *string
		if row.ImageUrl.Valid {
			imageURL = &row.ImageUrl.String
		}

		products[i] = EventProductRow{
			ID:            row.ID.String(),
			Name:          row.Name,
			ImageURL:      imageURL,
			Keyword:       row.Keyword,
			TotalQuantity: int(row.TotalQuantity),
			TotalRevenue:  row.TotalRevenue,
		}
	}

	return products, nil
}

// =============================================================================
// MÉTRICA EM DOIS NÍVEIS (Fatia 5)
//
// GetSessionStats saiu daqui: agrupava por carts.session_id (a sessão em que o
// carrinho NASCEU) e era chamada uma vez por sessão dentro do laço do evento.
// As três leituras abaixo são POR EVENTO — uma chamada cada, não N.
// =============================================================================

// MetricCutover é o instante em que uma métrica mudou de definição (D26).
type MetricCutover struct {
	Key         string
	EffectiveAt time.Time
	Note        string
}

// GetMetricCutover lê o marcador de corte. Marcador AUSENTE não é erro: a
// métrica continua respondendo, só sem a ressalva. Derrubar o relatório inteiro
// porque a nota de rodapé sumiu seria trocar um aviso por uma tela em branco.
func (r *Repository) GetMetricCutover(ctx context.Context, key string) (*MetricCutover, error) {
	row, err := r.q.GetMetricCutover(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting metric cutover %q: %w", key, err)
	}
	return &MetricCutover{
		Key:         row.Key,
		EffectiveAt: row.EffectiveAt.Time,
		Note:        row.Note,
	}, nil
}

// SessionConfirmedRow é o confirmado de uma transmissão. SessionID vazio é o
// balde "sem transmissão".
type SessionConfirmedRow struct {
	SessionID    string
	SoldUnits    int
	RevenueCents int64
	PaidCarts    int
}

// ListSessionConfirmedRevenueByEvent devolve a receita CONGELADA repartida por
// transmissão, direto de order_items (RN-13).
func (r *Repository) ListSessionConfirmedRevenueByEvent(ctx context.Context, eventID string) ([]SessionConfirmedRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListSessionConfirmedRevenueByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing session confirmed revenue: %w", err)
	}

	out := make([]SessionConfirmedRow, len(rows))
	for i, row := range rows {
		var sessionID string
		if row.SessionID.Valid {
			sessionID = row.SessionID.String()
		}
		out[i] = SessionConfirmedRow{
			SessionID:    sessionID,
			SoldUnits:    int(row.SoldUnits),
			RevenueCents: row.RevenueCents,
			PaidCarts:    int(row.PaidCarts),
		}
	}
	return out, nil
}

// ListProjectionInputByEvent devolve as duas metades do projetado: a quantidade
// final de cada item dos carrinhos abertos e o log de adições desses mesmos
// carrinhos. Quem junta é ProjectBySession — o repositório não faz conta.
func (r *Repository) ListProjectionInputByEvent(ctx context.Context, eventID string) ([]OpenCartItem, []CartItemAdditionRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, nil, err
	}

	itemRows, err := r.q.ListOpenCartItemsByEvent(ctx, uid)
	if err != nil {
		return nil, nil, fmt.Errorf("listing open cart items: %w", err)
	}
	items := make([]OpenCartItem, len(itemRows))
	for i, row := range itemRows {
		items[i] = OpenCartItem{
			CartID:    row.CartID.String(),
			ProductID: row.ProductID.String(),
			Quantity:  int(row.Quantity.Int32),
			UnitPrice: row.UnitPrice.Int64,
		}
	}

	logRows, err := r.q.ListCartItemEventsByEvent(ctx, uid)
	if err != nil {
		return nil, nil, fmt.Errorf("listing cart item events: %w", err)
	}
	additions := make([]CartItemAdditionRow, len(logRows))
	for i, row := range logRows {
		var sessionID string
		if row.SessionID.Valid {
			sessionID = row.SessionID.String()
		}
		additions[i] = CartItemAdditionRow{
			CartID:    row.CartID.String(),
			ProductID: row.ProductID.String(),
			CartItemAddition: CartItemAddition{
				SessionID: sessionID,
				Quantity:  int(row.Quantity),
				UnitPrice: row.UnitPrice,
			},
		}
	}

	return items, additions, nil
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// =============================================================================
// MODO LIVE (D17) — estado EFÊMERO de execução, agora na SESSÃO
//
// As colunas equivalentes em live_events saíram na 000122: quem manda é a
// sessão. As funções "…ForEvent" abaixo sustentam a rota legada do painel, que
// ainda só conhece o eventId, e aplicam o estado em TODAS as sessões vivas do
// evento.
// =============================================================================

// SetSessionActiveProduct define (ou limpa, com productID nil) o produto em
// destaque DAQUELA transmissão.
func (r *Repository) SetSessionActiveProduct(ctx context.Context, sessionID, storeID string, productID *string) (SessionRow, error) {
	sessionUID, err := parseUUID(sessionID)
	if err != nil {
		return SessionRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return SessionRow{}, err
	}
	var productUID pgtype.UUID
	if productID != nil && *productID != "" {
		productUID, err = parseUUID(*productID)
		if err != nil {
			return SessionRow{}, err
		}
	}

	row, err := r.q.SetSessionActiveProduct(ctx, sqlc.SetSessionActiveProductParams{
		ID:                     sessionUID,
		CurrentActiveProductID: productUID,
		StoreID:                storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRow{}, httpx.ErrNotFound("session not found")
		}
		return SessionRow{}, fmt.Errorf("setting session active product: %w", err)
	}
	return toSessionRow(row), nil
}

// SetSessionProcessingPaused pausa ou retoma o processamento DAQUELA transmissão.
func (r *Repository) SetSessionProcessingPaused(ctx context.Context, sessionID, storeID string, paused bool) (SessionRow, error) {
	sessionUID, err := parseUUID(sessionID)
	if err != nil {
		return SessionRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return SessionRow{}, err
	}

	row, err := r.q.SetSessionProcessingPaused(ctx, sqlc.SetSessionProcessingPausedParams{
		ID:               sessionUID,
		ProcessingPaused: paused,
		StoreID:          storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRow{}, httpx.ErrNotFound("session not found")
		}
		return SessionRow{}, fmt.Errorf("setting session processing paused: %w", err)
	}
	return toSessionRow(row), nil
}

// GetSessionLiveModeState devolve o estado do modo live DAQUELA transmissão.
func (r *Repository) GetSessionLiveModeState(ctx context.Context, sessionID, storeID string) (*LiveModeStateOutput, error) {
	sessionUID, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetSessionLiveModeState(ctx, sqlc.GetSessionLiveModeStateParams{
		ID:      sessionUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("session not found")
		}
		return nil, fmt.Errorf("getting session live mode state: %w", err)
	}

	return buildLiveModeState(
		row.ID.String(), row.ProcessingPaused, row.CurrentActiveProductID,
		row.ActiveProductName, row.ActiveProductKeyword,
		row.ActiveProductPrice, row.ActiveProductImageUrl,
	), nil
}

// SetActiveProductForEventSessions é a rota LEGADA: aplica o produto em
// destaque em todas as sessões VIVAS do evento.
func (r *Repository) SetActiveProductForEventSessions(ctx context.Context, eventID, storeID string, productID *string) error {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	var productUID pgtype.UUID
	if productID != nil && *productID != "" {
		productUID, err = parseUUID(*productID)
		if err != nil {
			return err
		}
	}
	return r.q.SetLiveModeForEventSessions(ctx, sqlc.SetLiveModeForEventSessionsParams{
		ID:                     eventUID,
		CurrentActiveProductID: productUID,
		StoreID:                storeUID,
	})
}

// SetProcessingPausedForEventSessions é a rota LEGADA da pausa.
func (r *Repository) SetProcessingPausedForEventSessions(ctx context.Context, eventID, storeID string, paused bool) error {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	return r.q.SetProcessingPausedForEventSessions(ctx, sqlc.SetProcessingPausedForEventSessionsParams{
		ID:               eventUID,
		ProcessingPaused: paused,
		StoreID:          storeUID,
	})
}

// GetLiveModeState devolve o estado do EVENTO lido da sessão viva mais recente.
func (r *Repository) GetLiveModeState(ctx context.Context, eventID, storeID string) (*LiveModeStateOutput, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetEventLiveModeStateFromSessions(ctx, sqlc.GetEventLiveModeStateFromSessionsParams{
		ID:      eventUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Evento sem sessão nenhuma: estado neutro, não 404 — o painel não
			// deve quebrar por causa de um evento que ainda não tem transmissão.
			return &LiveModeStateOutput{}, nil
		}
		return nil, fmt.Errorf("getting live mode state: %w", err)
	}

	return buildLiveModeState(
		row.SessionID.String(), row.ProcessingPaused, row.CurrentActiveProductID,
		row.ActiveProductName, row.ActiveProductKeyword,
		row.ActiveProductPrice, row.ActiveProductImageUrl,
	), nil
}

// buildLiveModeState centraliza a montagem do estado a partir das duas leituras
// (por sessão e a legada por evento) — elas devolvem exatamente as mesmas
// colunas.
func buildLiveModeState(
	sessionID string,
	paused bool,
	productID pgtype.UUID,
	name, keyword pgtype.Text,
	price pgtype.Int8,
	imageURL pgtype.Text,
) *LiveModeStateOutput {
	out := &LiveModeStateOutput{SessionID: sessionID, ProcessingPaused: paused}
	// name inválido = produto apagado depois de destacado; sem ele não há o que
	// mostrar, e devolver só o id enganaria o painel.
	if productID.Valid && name.Valid {
		var image *string
		if imageURL.Valid {
			image = &imageURL.String
		}
		out.ActiveProduct = &ActiveProductOutput{
			ID:       productID.String(),
			Name:     name.String,
			Keyword:  keyword.String,
			Price:    price.Int64,
			ImageURL: image,
		}
	}
	return out
}

// A camada de event_products saiu daqui: a whitelist passou a ser da SESSÃO
// (000112). As operações equivalentes estão em session_product_repository.go, e
// as rotas legadas por evento são traduzidas para todas as sessões do evento.
// A tabela event_products saiu do banco na 000122: ela sobrevivia só como
// instantâneo de rollback e ninguém mais a lia nem escrevia.

// =============================================================================
// EVENT UPSELLS
// =============================================================================

// AddEventUpsell adds an upsell to an event
func (r *Repository) AddEventUpsell(ctx context.Context, input AddEventUpsellInput) (EventUpsellOutput, error) {
	eventUID, err := parseUUID(input.EventID)
	if err != nil {
		return EventUpsellOutput{}, err
	}
	productUID, err := parseUUID(input.ProductID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	var messageTemplate pgtype.Text
	if input.MessageTemplate != nil {
		messageTemplate = pgtype.Text{String: *input.MessageTemplate, Valid: true}
	}

	// Use transaction to ensure atomicity (INSERT + SELECT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	created, err := qtx.CreateEventUpsell(ctx, sqlc.CreateEventUpsellParams{
		EventID:         eventUID,
		ProductID:       productUID,
		DiscountPercent: input.DiscountPercent,
		MessageTemplate: messageTemplate,
		DisplayOrder:    input.DisplayOrder,
		Active:          input.Active,
	})
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("adding event upsell: %w", err)
	}

	// Get with joined product data
	row, err := qtx.GetEventUpsellByID(ctx, created.ID)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("getting created event upsell: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventUpsellOutput{}, fmt.Errorf("committing transaction: %w", err)
	}

	return toEventUpsellOutputFromGet(row), nil
}

// ListEventUpsells returns all upsells for an event
func (r *Repository) ListEventUpsells(ctx context.Context, eventID string) ([]EventUpsellOutput, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListEventUpsells(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing event upsells: %w", err)
	}

	upsells := make([]EventUpsellOutput, len(rows))
	for i, row := range rows {
		upsells[i] = toEventUpsellOutputFromList(row)
	}

	return upsells, nil
}

// UpdateEventUpsell updates an upsell's configuration
func (r *Repository) UpdateEventUpsell(ctx context.Context, input UpdateEventUpsellInput) (EventUpsellOutput, error) {
	uid, err := parseUUID(input.ID)
	if err != nil {
		return EventUpsellOutput{}, err
	}
	eventUID, err := parseUUID(input.EventID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	var messageTemplate pgtype.Text
	if input.MessageTemplate != nil {
		messageTemplate = pgtype.Text{String: *input.MessageTemplate, Valid: true}
	}

	// Use transaction to ensure atomicity (UPDATE + SELECT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	updated, err := qtx.UpdateEventUpsell(ctx, sqlc.UpdateEventUpsellParams{
		ID:              uid,
		EventID:         eventUID,
		DiscountPercent: input.DiscountPercent,
		MessageTemplate: messageTemplate,
		DisplayOrder:    input.DisplayOrder,
		Active:          input.Active,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventUpsellOutput{}, httpx.ErrNotFound("event upsell not found")
		}
		return EventUpsellOutput{}, fmt.Errorf("updating event upsell: %w", err)
	}

	// Get with joined product data
	row, err := qtx.GetEventUpsellByID(ctx, updated.ID)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("getting updated event upsell: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventUpsellOutput{}, fmt.Errorf("committing transaction: %w", err)
	}

	return toEventUpsellOutputFromGet(row), nil
}

// DeleteEventUpsell removes an upsell from an event
func (r *Repository) DeleteEventUpsell(ctx context.Context, id, eventID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return err
	}

	return r.q.DeleteEventUpsell(ctx, sqlc.DeleteEventUpsellParams{
		ID:      uid,
		EventID: eventUID,
	})
}

// CountEventUpsells returns the number of upsells for an event
func (r *Repository) CountEventUpsells(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	count, err := r.q.CountEventUpsells(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting event upsells: %w", err)
	}

	return int(count), nil
}

// GetEventWithCounts returns an event with its upsell count.
//
// A contagem de PRODUTOS saiu: não existe "quantos produtos a campanha vende".
// A lista é da transmissão, e quem responde isso é CountSessionProductsByEvent,
// uma contagem por sessão.
func (r *Repository) GetEventWithCounts(ctx context.Context, eventID, storeID string) (*EventRow, int, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, 0, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, 0, err
	}

	row, err := r.q.GetLiveEventWithCounts(ctx, sqlc.GetLiveEventWithCountsParams{
		ID:      eventUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, httpx.ErrNotFound("event not found")
		}
		return nil, 0, fmt.Errorf("getting event with counts: %w", err)
	}

	eventRow := toEventRowFromWithCounts(row)
	return &eventRow, int(row.UpsellCount), nil
}

// =============================================================================
// EVENT PRODUCT/UPSELL HELPERS
// =============================================================================

func toEventUpsellOutputFromGet(row sqlc.GetEventUpsellByIDRow) EventUpsellOutput {
	var messageTemplate *string
	if row.MessageTemplate.Valid {
		messageTemplate = &row.MessageTemplate.String
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	discountedPrice := row.OriginalPrice.Int64 * int64(100-row.DiscountPercent) / 100

	return EventUpsellOutput{
		ID:              row.ID.String(),
		ProductID:       row.ProductID.String(),
		Name:            row.ProductName,
		Keyword:         row.ProductKeyword,
		ImageURL:        imageURL,
		OriginalPrice:   row.OriginalPrice.Int64,
		DiscountPercent: row.DiscountPercent,
		DiscountedPrice: discountedPrice,
		MessageTemplate: messageTemplate,
		DisplayOrder:    row.DisplayOrder,
		Active:          row.Active,
		Stock:           row.ProductStock.Int32,
		CreatedAt:       row.CreatedAt.Time,
		UpdatedAt:       row.UpdatedAt.Time,
	}
}

func toEventUpsellOutputFromList(row sqlc.ListEventUpsellsRow) EventUpsellOutput {
	var messageTemplate *string
	if row.MessageTemplate.Valid {
		messageTemplate = &row.MessageTemplate.String
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	discountedPrice := row.OriginalPrice.Int64 * int64(100-row.DiscountPercent) / 100

	return EventUpsellOutput{
		ID:              row.ID.String(),
		ProductID:       row.ProductID.String(),
		Name:            row.ProductName,
		Keyword:         row.ProductKeyword,
		ImageURL:        imageURL,
		OriginalPrice:   row.OriginalPrice.Int64,
		DiscountPercent: row.DiscountPercent,
		DiscountedPrice: discountedPrice,
		MessageTemplate: messageTemplate,
		DisplayOrder:    row.DisplayOrder,
		Active:          row.Active,
		Stock:           row.ProductStock.Int32,
		CreatedAt:       row.CreatedAt.Time,
		UpdatedAt:       row.UpdatedAt.Time,
	}
}

func toEventRowFromWithCounts(row sqlc.GetLiveEventWithCountsRow) EventRow {
	var title string
	if row.Title.Valid {
		title = row.Title.String
	}

	var cartExpirationMinutes, cartMaxQuantityPerItem *int
	if row.CartExpirationMinutes.Valid {
		v := int(row.CartExpirationMinutes.Int32)
		cartExpirationMinutes = &v
	}
	if row.CartMaxQuantityPerItem.Valid {
		v := int(row.CartMaxQuantityPerItem.Int32)
		cartMaxQuantityPerItem = &v
	}
	var autoSendCheckoutLinks *bool
	if row.SendOnLiveEnd.Valid {
		autoSendCheckoutLinks = &row.SendOnLiveEnd.Bool
	}
	var scheduledAt *time.Time
	if row.ScheduledAt.Valid {
		scheduledAt = &row.ScheduledAt.Time
	}
	var endsAt *time.Time
	if row.EndsAt.Valid {
		endsAt = &row.EndsAt.Time
	}
	var description *string
	if row.Description.Valid {
		description = &row.Description.String
	}

	return EventRow{
		ID:                     row.ID.String(),
		StoreID:                row.StoreID.String(),
		Title:                  title,
		Status:                 row.Status,
		TotalOrders:            int(row.TotalOrders),
		CloseCartOnEventEnd:    row.CloseCartOnEventEnd,
		CartExpirationMinutes:  cartExpirationMinutes,
		CartMaxQuantityPerItem: cartMaxQuantityPerItem,
		SendOnLiveEnd:          autoSendCheckoutLinks,
		ScheduledAt:            scheduledAt,
		EndsAt:                 endsAt,
		Description:            description,
		CreatedAt:              row.CreatedAt.Time,
		UpdatedAt:              row.UpdatedAt.Time,

		WaitlistNotifiedTTLMinutes: int(row.WaitlistNotifiedTtlMinutes),
	}
}

// SetSessionType grava a espécie da transmissão. Ver SetSessionType em
// db/queries/live.sql: a campanha nasce sem perguntar o tipo, e a sessão o
// aprende quando a publicação é vinculada.
func (r *Repository) SetSessionType(ctx context.Context, sessionID, sessionType string) error {
	sid, err := parseUUID(sessionID)
	if err != nil {
		return err
	}
	return r.q.SetSessionType(ctx, sqlc.SetSessionTypeParams{ID: sid, Type: sessionType})
}

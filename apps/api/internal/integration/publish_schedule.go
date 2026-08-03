package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/live"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Publicação agendada no Instagram (RN-31 / N3).
//
// A Graph API de Content Publishing NÃO tem agendamento nativo: não existe
// scheduled_publish_time, e o container de mídia expira em 24h. Então o
// agendador é nosso, e o container só é aberto no disparo — o que fica
// guardado até lá é a intenção (o asset, a legenda, os produtos, a janela).
//
// A diferença estrutural em relação ao publish síncrono é o ASSET: no caminho
// síncrono o arquivo é apagado do storage logo depois de publicar, porque o
// Instagram já o buscou. Aqui ele precisa sobreviver até a data — e a URL
// assinada NÃO, porque ela dura horas e o agendamento dura dias. O job guarda a
// chave; a URL é gerada no disparo.

const (
	// Lead mínimo entre agendar e publicar. Não é capricho: o disparo pode cair
	// no backstop de 5 minutos se a task ETA se perder, então prometer uma
	// publicação para "daqui a 30 segundos" seria prometer o que o mecanismo
	// não entrega. Quem quer publicar agora usa o caminho síncrono.
	minPublishLead = 5 * time.Minute

	// Horizonte máximo. O token de longa duração do Instagram vale 60 dias e é
	// renovado pelo uso; agendar além disso é agendar para um token que pode
	// não existir mais, e a falha só apareceria no dia.
	maxPublishHorizon = 60 * 24 * time.Hour

	// Tentativas antes do dead-letter. O asynq tenta com backoff próprio; este
	// teto é o do JOB, e é ele que decide quando o asset pode ser liberado.
	maxPublishAttempts = 5

	// Tempo depois do qual um job em 'publishing' é considerado abandonado
	// (processo morto entre a reivindicação e o desfecho). Folgado de propósito:
	// publicar um Reel envolve o processamento do vídeo pelo Instagram, e o
	// provider espera até 5 minutos pelo container.
	stuckPublishAfter = 15 * time.Minute

	// Validade da URL assinada gerada no disparo. Só precisa cobrir a janela
	// entre criar o container e o Instagram terminar de buscar a mídia.
	publishAssetURLTTL = 6 * time.Hour

	// Tamanho do lote do backstop. Pequeno de propósito: ele roda a cada 5
	// minutos e o que sobrar sai no ciclo seguinte — represar é preferível a
	// despejar um lote inteiro de publicações na fila de uma vez, porque a Meta
	// limita posts por API numa janela móvel de 24h.
	publishSweepBatch = 25
)

// PublishScheduler enfileira o disparo da publicação com ETA. Interface no
// domínio, adapter na composition root — mesmo desenho de CartExpiryScheduler
// e EventCloseScheduler.
type PublishScheduler interface {
	SchedulePublish(ctx context.Context, jobID string, at time.Time) error
	// CancelPublish remove a task pendente. Best-effort por contrato: o
	// cancelamento REAL é a linha do banco sair de 'scheduled' — se a task
	// disparar mesmo assim, ClaimPublishJob não acha nada e o disparo é um
	// no-op. Sem essa ordem, um Redis fora do ar tornaria o cancelamento
	// impossível.
	CancelPublish(ctx context.Context, jobID string) error
}

// SetPublishScheduler injeta o agendador ETA da publicação.
func (s *Service) SetPublishScheduler(p PublishScheduler) { s.publishScheduler = p }

// PublishJob é o agendamento como o domínio o enxerga.
type PublishJob struct {
	ID                     string
	StoreID                string
	SessionID              string
	EventID                string
	MediaKind              string
	AssetPath              string
	AssetContentType       string
	Caption                string
	Title                  string
	ProductIDs             []string
	StartsAt               *time.Time
	EndsAt                 *time.Time
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	ScheduledFor           time.Time
	Status                 string
	PublishedMediaID       string
	Attempts               int
	LastError              string
	PublishedAt            *time.Time
	CancelledAt            *time.Time
	CreatedAt              time.Time
}

// CreatePublishJobParams é o que o repositório grava.
type CreatePublishJobParams struct {
	StoreID                string
	MediaKind              string
	AssetPath              string
	AssetContentType       string
	Caption                string
	Title                  string
	ProductIDs             []string
	StartsAt               *time.Time
	EndsAt                 *time.Time
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	ScheduledFor           time.Time
}

// SchedulePublishInput é o pedido de agendamento vindo do painel. O asset já
// está no storage quando isto chega — o upload acontece no handler, como no
// caminho síncrono.
type SchedulePublishInput struct {
	StoreID                string
	MediaKind              string
	AssetPath              string
	AssetContentType       string
	Caption                string
	Title                  string
	ProductIDs             []string
	StartsAt               *time.Time
	EndsAt                 *time.Time
	CartExpirationMinutes  *int
	CartMaxQuantityPerItem *int
	ScheduledFor           time.Time
}

// SchedulePublish grava o agendamento e arma a task ETA.
//
// Nada é enviado ao Instagram aqui: o container expira em 24h e a publicação
// pode ser para semana que vem. O asset fica retido no storage e só é liberado
// no desfecho do job (publicado, falho ou cancelado).
func (s *Service) SchedulePublish(ctx context.Context, input SchedulePublishInput) (*PublishJob, error) {
	switch input.MediaKind {
	case "post", "reel", "story":
	default:
		return nil, httpx.ErrBadRequest("mediaKind must be post, reel or story")
	}
	if len(input.ProductIDs) == 0 {
		return nil, httpx.ErrBadRequest("select at least one product for the promotion")
	}
	if input.AssetPath == "" {
		return nil, httpx.ErrBadRequest("asset is required")
	}

	now := time.Now()
	if input.ScheduledFor.Before(now.Add(minPublishLead)) {
		return nil, httpx.ErrUnprocessable(
			fmt.Sprintf("scheduledFor must be at least %d minutes from now", int(minPublishLead.Minutes())))
	}
	if input.ScheduledFor.After(now.Add(maxPublishHorizon)) {
		return nil, httpx.ErrUnprocessable("scheduledFor is too far ahead (max 60 days)")
	}
	// A janela comercial é do EVENTO, que nasce no disparo — mas quem a informa
	// é o lojista agora, então a incoerência tem de ser recusada agora. Um
	// ends_at no passado no dia do disparo criaria um evento nascido encerrado.
	if input.EndsAt != nil && !input.EndsAt.After(input.ScheduledFor) {
		return nil, httpx.ErrUnprocessable("endsAt must be after the scheduled publication")
	}
	if input.StartsAt != nil && input.EndsAt != nil && !input.EndsAt.After(*input.StartsAt) {
		return nil, httpx.ErrUnprocessable("endsAt must be after startsAt")
	}
	// Story não tem janela declarada: o Instagram o derruba em 24h e o evento é
	// criado com ends_at = publicação + 24h (mesma regra do caminho síncrono).
	// Aceitar uma janela aqui seria aceitar um valor que o disparo ignora.
	if input.MediaKind == "story" && (input.StartsAt != nil || input.EndsAt != nil) {
		return nil, httpx.DomainError(422, httpx.CodeIgStoryNoWindow, "a story has no commercial window — it lasts 24h from publication")
	}

	job, err := s.repo.CreatePublishJob(ctx, CreatePublishJobParams(input))
	if err != nil {
		return nil, err
	}

	s.armPublish(ctx, job.ID, job.ScheduledFor)

	logger.From(ctx, s.logger).Info("instagram publication scheduled",
		zap.String("store_id", input.StoreID),
		zap.String("job_id", job.ID),
		zap.String("media_kind", input.MediaKind),
		zap.Time("scheduled_for", job.ScheduledFor),
	)
	return job, nil
}

// armPublish arma a task ETA. Best-effort de propósito: falhar o agendamento
// porque o Redis está fora do ar seria perder o pedido do lojista, enquanto o
// sweep de vencidos alcança o job no ciclo seguinte com atraso de minutos.
func (s *Service) armPublish(ctx context.Context, jobID string, at time.Time) {
	if s.publishScheduler == nil {
		logger.From(ctx, s.logger).Warn("publish scheduler not wired — job will run from the sweep",
			zap.String("job_id", jobID))
		return
	}
	if err := s.publishScheduler.SchedulePublish(ctx, jobID, at); err != nil {
		logger.From(ctx, s.logger).Warn("failed to arm scheduled publication",
			zap.String("job_id", jobID), zap.Error(err))
	}
}

// ListPublishJobs lista os agendamentos da loja para a tela de gerenciamento.
func (s *Service) ListPublishJobs(ctx context.Context, storeID, status string, limit int) ([]PublishJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return s.repo.ListPublishJobs(ctx, storeID, status, int32(limit))
}

// CancelPublish cancela um agendamento ainda não disparado.
//
// A ordem importa: primeiro o banco, depois a fila. A linha sair de 'scheduled'
// é o cancelamento de verdade — se a task ainda vier, ClaimPublishJob não acha
// nada e o disparo é um no-op. Apagar a task primeiro e falhar no banco
// deixaria um agendamento vivo sem ninguém para dispará-lo.
func (s *Service) CancelPublish(ctx context.Context, jobID, storeID string) (*PublishJob, error) {
	job, err := s.repo.CancelPublishJob(ctx, jobID, storeID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		// O UPDATE guardado não achou linha: ou o job não é desta loja / não
		// existe, ou já saiu de 'scheduled'. A leitura distingue os dois casos,
		// porque "já publicado" e "não existe" pedem respostas diferentes.
		existing, readErr := s.repo.GetPublishJob(ctx, jobID, storeID)
		if readErr != nil {
			return nil, readErr
		}
		if existing == nil {
			return nil, httpx.ErrNotFound("scheduled publication not found")
		}
		if existing.Status == "publishing" {
			return nil, httpx.DomainError(422, httpx.CodeIgPublishInFlight, "this publication is being sent to Instagram right now and can no longer be cancelled")
		}
		return nil, httpx.ErrUnprocessable(fmt.Sprintf("this publication is already %s", existing.Status))
	}

	if s.publishScheduler != nil {
		if err := s.publishScheduler.CancelPublish(ctx, jobID); err != nil {
			// Não é falha do cancelamento: o guard do banco já o garantiu.
			logger.From(ctx, s.logger).Warn("failed to drop the pending publish task (the DB guard already cancels it)",
				zap.String("job_id", jobID), zap.Error(err))
		}
	}

	// O asset só existia para esta publicação. Cancelou, não há o que reter.
	s.deleteTransientImage(ctx, job.AssetPath)

	logger.From(ctx, s.logger).Info("scheduled publication cancelled",
		zap.String("store_id", storeID), zap.String("job_id", jobID))
	return job, nil
}

// RunScheduledPublish é o disparo. Guard-first, como RunScheduledExpiry e
// RunScheduledEventClose: a reivindicação atômica decide se há algo a fazer.
//
// O erro devolvido é o sinal de retry para o asynq. nil significa "terminou,
// não tente de novo" — inclusive nos desfechos ruins que não adianta repetir
// (job cancelado, tentativas esgotadas, post já no ar).
func (s *Service) RunScheduledPublish(ctx context.Context, jobID string) error {
	job, err := s.repo.ClaimPublishJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		// Cancelado, já publicado, ou entrega duplicada da mesma task.
		logger.From(ctx, s.logger).Info("scheduled publication skipped (not claimable)",
			zap.String("job_id", jobID))
		return nil
	}

	mediaID, pubErr := s.publishJobMedia(ctx, job)
	if pubErr != nil {
		return s.handlePublishFailure(ctx, job, pubErr)
	}

	// Daqui em diante o post está NO AR. Nada abaixo pode devolver erro de
	// retry: repetir republicaria a mídia.
	eventID, sessionID, bindErr := s.bindPublishedMedia(ctx, job, mediaID)
	bindMsg := ""
	if bindErr != nil {
		bindMsg = truncateError(bindErr.Error())
		logger.From(ctx, s.logger).Error("scheduled publication went live but the event could not be created",
			zap.String("job_id", job.ID),
			zap.String("media_id", mediaID),
			zap.Error(bindErr))
	}
	if err := s.repo.MarkPublishJobPublished(ctx, job.ID, mediaID, eventID, sessionID, bindMsg); err != nil {
		logger.From(ctx, s.logger).Error("failed to close the publish job as published",
			zap.String("job_id", job.ID), zap.String("media_id", mediaID), zap.Error(err))
	}

	// O Instagram já buscou a mídia; o asset retido pode sair.
	s.deleteTransientImage(ctx, job.AssetPath)

	logger.From(ctx, s.logger).Info("scheduled publication published",
		zap.String("store_id", job.StoreID),
		zap.String("job_id", job.ID),
		zap.String("media_kind", job.MediaKind),
		zap.String("media_id", mediaID),
		zap.String("event_id", eventID),
	)
	return nil
}

// publishJobMedia re-assina a URL do asset retido e publica pelo caminho da
// espécie. A URL é gerada AGORA porque a do agendamento já expirou faz dias —
// era esse o buraco que impedia a publicação agendada de existir.
func (s *Service) publishJobMedia(ctx context.Context, job *PublishJob) (string, error) {
	if s.storage == nil {
		return "", errors.New("storage not wired — cannot resolve the retained asset")
	}
	assetURL, err := s.storage.GeneratePresignedGetURL(ctx, job.AssetPath, publishAssetURLTTL)
	if err != nil {
		return "", fmt.Errorf("signing the retained asset: %w", err)
	}
	if assetURL == "" {
		return "", fmt.Errorf("the retained asset is gone from storage (%s)", job.AssetPath)
	}

	// Relê a integração e decripta as credenciais AGORA, renovando o token se
	// ele venceu — é por isso que o disparo não usa um provider capturado no
	// agendamento (CA-31.5).
	provider, err := s.resolveInstagramSocialProvider(ctx, job.StoreID)
	if err != nil {
		return "", err
	}

	switch job.MediaKind {
	case "post":
		return provider.PublishImagePost(ctx, assetURL, job.Caption)
	case "reel":
		return provider.PublishReel(ctx, assetURL, job.Caption)
	case "story":
		return provider.PublishStory(ctx, assetURL, instagramAssetIsVideo(job.AssetContentType))
	default:
		return "", fmt.Errorf("unknown media kind %q", job.MediaKind)
	}
}

// bindPublishedMedia cria o evento de post-commerce da mídia recém-publicada e
// amarra a sessão ao agendamento. Reusa CreatePostEvent — o mesmo caminho do
// publish síncrono e do "selecionar um post".
func (s *Service) bindPublishedMedia(ctx context.Context, job *PublishJob, mediaID string) (eventID, sessionID string, err error) {
	permalink, thumbnail := "", ""
	if provider, pErr := s.resolveInstagramSocialProvider(ctx, job.StoreID); pErr == nil {
		if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
			permalink = details.Permalink
			thumbnail = details.ThumbnailURL
			if thumbnail == "" {
				thumbnail = details.MediaURL
			}
		}
	}

	input := live.CreatePostInput{
		StoreID:                job.StoreID,
		Title:                  job.Title,
		MediaID:                mediaID,
		MediaPermalink:         permalink,
		MediaThumbnailURL:      thumbnail,
		MediaCaption:           job.Caption,
		ProductIDs:             job.ProductIDs,
		StartsAt:               job.StartsAt,
		EndsAt:                 job.EndsAt,
		CartExpirationMinutes:  job.CartExpirationMinutes,
		CartMaxQuantityPerItem: job.CartMaxQuantityPerItem,
	}
	switch job.MediaKind {
	case "reel":
		input.Type = live.SessionTypeReel
	case "story":
		input.Type = "story"
		// O Story morre em 24h contadas da PUBLICAÇÃO, não do agendamento —
		// por isso o cálculo mora aqui e não em SchedulePublish.
		endsAt := time.Now().Add(24 * time.Hour)
		input.EndsAt = &endsAt
	}

	out, err := s.liveService.CreatePostEvent(ctx, input)
	if err != nil {
		return "", "", err
	}

	sessionID, sErr := s.liveService.MarkSessionPublished(ctx, mediaID, job.ScheduledFor)
	if sErr != nil {
		// A sessão existe; só o carimbo de publish_at não entrou. Não é motivo
		// para o job inteiro parecer falho.
		logger.From(ctx, s.logger).Warn("failed to stamp publish_at on the session",
			zap.String("job_id", job.ID), zap.String("media_id", mediaID), zap.Error(sErr))
	}
	return out.ID, sessionID, nil
}

// handlePublishFailure decide entre retry e dead-letter.
//
// O teto é do JOB, não do asynq: é o número de tentativas gravado que diz
// quando parar, porque o backstop também dispara e não passa pelo contador de
// retry da fila. Enquanto houver tentativa, o job volta para 'scheduled' — o
// único estado que a reivindicação enxerga.
func (s *Service) handlePublishFailure(ctx context.Context, job *PublishJob, pubErr error) error {
	msg := truncateError(pubErr.Error())
	if job.Attempts >= maxPublishAttempts {
		if err := s.repo.FailPublishJob(ctx, job.ID, msg); err != nil {
			logger.From(ctx, s.logger).Error("failed to dead-letter the publish job",
				zap.String("job_id", job.ID), zap.Error(err))
		}
		// Ninguém mais vai tentar: o asset retido perdeu a razão de existir.
		s.deleteTransientImage(ctx, job.AssetPath)
		logger.From(ctx, s.logger).Error("scheduled publication dead-lettered",
			zap.String("store_id", job.StoreID),
			zap.String("job_id", job.ID),
			zap.Int("attempts", job.Attempts),
			zap.Error(pubErr))
		// nil: o asynq não deve tentar de novo o que o domínio já encerrou.
		return nil
	}

	if err := s.repo.ReleasePublishJob(ctx, job.ID, msg); err != nil {
		logger.From(ctx, s.logger).Error("failed to release the publish job for retry",
			zap.String("job_id", job.ID), zap.Error(err))
	}
	logger.From(ctx, s.logger).Warn("scheduled publication failed, will retry",
		zap.String("store_id", job.StoreID),
		zap.String("job_id", job.ID),
		zap.Int("attempts", job.Attempts),
		zap.Error(pubErr))
	return pubErr
}

// SweepDuePublishJobs é a rede do agendador ETA, no mesmo papel do
// SweepEndedTimedEvents: task perdida (Redis limpo, deploy no meio da janela)
// deixaria o agendamento vencido e ninguém o dispararia — e a falha seria
// silenciosa, que é o pior desfecho possível para uma publicação prometida.
//
// Faz duas coisas: devolve à fila o que ficou preso em 'publishing' porque o
// processo morreu no meio, e RE-ARMA o que venceu.
//
// Re-armar, e não publicar aqui dentro, é decisão deliberada. Este sweep roda
// no mesmo goroutine do ticker de 5 minutos que carrega o sweep do ERP e os de
// evento, e publicar um Reel BLOQUEIA por até 5 minutos por job — o provider
// espera o Instagram terminar de processar o vídeo. Um lote de agendamentos
// vencidos travaria os outros sweeps por horas. O trabalho pesado tem de rodar
// no pool do asynq, que é onde ele já roda no caminho normal.
//
// De brinde, o TaskID dedupe melhor que qualquer guard nosso: se a task ainda
// existe pendente, o Schedule devolve ErrTaskIDConflict e o sweep não cria uma
// segunda.
func (s *Service) SweepDuePublishJobs(ctx context.Context) {
	stuck, err := s.repo.ListStuckPublishJobs(ctx, time.Now().Add(-stuckPublishAfter), publishSweepBatch)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list stuck publish jobs", zap.Error(err))
	}
	for _, job := range stuck {
		if err := s.repo.ReleasePublishJob(ctx, job.ID, "abandoned mid-publish; requeued by the sweep"); err != nil {
			logger.From(ctx, s.logger).Error("failed to requeue a stuck publish job",
				zap.String("job_id", job.ID), zap.Error(err))
			continue
		}
		logger.From(ctx, s.logger).Warn("stuck publish job requeued",
			zap.String("job_id", job.ID), zap.Int("attempts", job.Attempts))
	}

	// O lote inclui os que acabaram de ser soltos acima: o scheduled_for deles
	// já passou, então eles saem no mesmo ciclo em vez de esperar o próximo.
	due, err := s.repo.ListDuePublishJobs(ctx, publishSweepBatch)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list due publish jobs", zap.Error(err))
		return
	}
	for _, job := range due {
		jobCtx := logger.WithStore(ctx, job.StoreID, "")
		if s.publishScheduler == nil {
			// Sem agendador não há para onde empurrar. Publicar aqui é ruim
			// (bloqueia o ticker), mas é menos ruim que a publicação nunca
			// sair — e este ramo só existe em instalação sem fila.
			logger.From(jobCtx, s.logger).Warn("publishing inline: no scheduler wired",
				zap.String("job_id", job.ID))
			if err := s.RunScheduledPublish(jobCtx, job.ID); err != nil {
				logger.From(jobCtx, s.logger).Warn("inline run of a scheduled publication failed",
					zap.String("job_id", job.ID), zap.Error(err))
			}
			continue
		}
		// ProcessAt = agora: o pool do asynq pega na sequência.
		if err := s.publishScheduler.SchedulePublish(jobCtx, job.ID, time.Now()); err != nil {
			logger.From(jobCtx, s.logger).Error("failed to re-arm an overdue publication",
				zap.String("job_id", job.ID), zap.Error(err))
			continue
		}
		logger.From(jobCtx, s.logger).Warn("overdue publication re-armed by the sweep",
			zap.String("job_id", job.ID), zap.Time("scheduled_for", job.ScheduledFor))
	}
}

// truncateError encurta a mensagem gravada em last_error: o corpo de erro da
// Graph vem inteiro e a coluna é lida por humano, não por parser.
func truncateError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}

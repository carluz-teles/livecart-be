package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

// Persistência do agendamento de publicação (RN-31 / 000121).
//
// Tudo aqui converte a linha crua num PublishJob de domínio: o resto do pacote
// não conhece pgtype, e o disparo trabalha com []string de produto porque é
// esse o formato que CreatePostEvent consome.

// CreatePublishJob grava a INTENÇÃO de publicar. Nada é enviado ao Instagram
// neste ponto — o container da Graph expira em 24h e o agendamento pode ser
// para semana que vem.
func (r *Repository) CreatePublishJob(ctx context.Context, in CreatePublishJobParams) (*PublishJob, error) {
	storeUID, err := parseUUID(in.StoreID)
	if err != nil {
		return nil, err
	}
	productUIDs := make([]pgtype.UUID, 0, len(in.ProductIDs))
	for _, id := range in.ProductIDs {
		uid, err := parseUUID(id)
		if err != nil {
			return nil, err
		}
		productUIDs = append(productUIDs, uid)
	}

	row, err := r.queries.CreateSessionPublishJob(ctx, sqlc.CreateSessionPublishJobParams{
		StoreID:                storeUID,
		MediaKind:              in.MediaKind,
		AssetPath:              in.AssetPath,
		AssetContentType:       in.AssetContentType,
		Caption:                in.Caption,
		Title:                  in.Title,
		ProductIds:             productUIDs,
		StartsAt:               optionalTimestamptz(in.StartsAt),
		EndsAt:                 optionalTimestamptz(in.EndsAt),
		CartExpirationMinutes:  optionalInt4(in.CartExpirationMinutes),
		CartMaxQuantityPerItem: optionalInt4(in.CartMaxQuantityPerItem),
		ScheduledFor:           pgtype.Timestamptz{Time: in.ScheduledFor, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("creating publish job: %w", err)
	}
	job := toPublishJob(row)
	return &job, nil
}

// GetPublishJob lê um agendamento no escopo da loja. nil quando não existe —
// o handler traduz para 404.
func (r *Repository) GetPublishJob(ctx context.Context, jobID, storeID string) (*PublishJob, error) {
	jobUID, err := parseUUID(jobID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetSessionPublishJob(ctx, sqlc.GetSessionPublishJobParams{ID: jobUID, StoreID: storeUID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting publish job: %w", err)
	}
	job := toPublishJob(row)
	return &job, nil
}

// ListPublishJobs devolve os agendamentos da loja, do mais recente para o mais
// antigo. status vazio = todos.
func (r *Repository) ListPublishJobs(ctx context.Context, storeID, status string, limit int32) ([]PublishJob, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListSessionPublishJobsByStore(ctx, sqlc.ListSessionPublishJobsByStoreParams{
		StoreID: storeUID,
		Status:  pgtype.Text{String: status, Valid: status != ""},
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing publish jobs: %w", err)
	}
	out := make([]PublishJob, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPublishJob(row))
	}
	return out, nil
}

// ClaimPublishJob reivindica o job para ESTE disparo. Devolve nil quando o job
// não está mais 'scheduled' — cancelado, já publicado, ou reivindicado por uma
// entrega duplicada da mesma task. É o guard que torna a task at-least-once
// segura, e é também o que faz o cancelamento valer depois de a task já ter
// sido entregue.
func (r *Repository) ClaimPublishJob(ctx context.Context, jobID string) (*PublishJob, error) {
	jobUID, err := parseUUID(jobID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.ClaimSessionPublishJob(ctx, jobUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claiming publish job: %w", err)
	}
	job := toPublishJob(row)
	return &job, nil
}

// MarkPublishJobPublished fecha o job como publicado. bindErr é preenchido no
// desfecho "publicou mas não deu para criar o evento": o post está no ar, o
// job NÃO pode voltar para a fila, e o motivo precisa sobreviver.
func (r *Repository) MarkPublishJobPublished(ctx context.Context, jobID, mediaID, eventID, sessionID, bindErr string) error {
	jobUID, err := parseUUID(jobID)
	if err != nil {
		return err
	}
	params := sqlc.MarkSessionPublishJobPublishedParams{
		ID:               jobUID,
		PublishedMediaID: pgtype.Text{String: mediaID, Valid: mediaID != ""},
		LastError:        pgtype.Text{String: bindErr, Valid: bindErr != ""},
	}
	if eventID != "" {
		uid, err := parseUUID(eventID)
		if err != nil {
			return err
		}
		params.EventID = uid
	}
	if sessionID != "" {
		uid, err := parseUUID(sessionID)
		if err != nil {
			return err
		}
		params.SessionID = uid
	}
	return r.queries.MarkSessionPublishJobPublished(ctx, params)
}

// ReleasePublishJob devolve o job para 'scheduled' depois de uma tentativa que
// falhou e ainda tem retry.
func (r *Repository) ReleasePublishJob(ctx context.Context, jobID, lastErr string) error {
	jobUID, err := parseUUID(jobID)
	if err != nil {
		return err
	}
	return r.queries.ReleaseSessionPublishJob(ctx, sqlc.ReleaseSessionPublishJobParams{
		ID:        jobUID,
		LastError: pgtype.Text{String: lastErr, Valid: lastErr != ""},
	})
}

// FailPublishJob é o dead-letter: tentativas esgotadas, ninguém vai tentar de
// novo sozinho.
func (r *Repository) FailPublishJob(ctx context.Context, jobID, lastErr string) error {
	jobUID, err := parseUUID(jobID)
	if err != nil {
		return err
	}
	return r.queries.FailSessionPublishJob(ctx, sqlc.FailSessionPublishJobParams{
		ID:        jobUID,
		LastError: pgtype.Text{String: lastErr, Valid: lastErr != ""},
	})
}

// CancelPublishJob cancela o agendamento. nil quando o job não está mais
// 'scheduled' — o handler traduz isso em 422 com a razão, não em 404.
func (r *Repository) CancelPublishJob(ctx context.Context, jobID, storeID string) (*PublishJob, error) {
	jobUID, err := parseUUID(jobID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.CancelSessionPublishJob(ctx, sqlc.CancelSessionPublishJobParams{ID: jobUID, StoreID: storeUID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cancelling publish job: %w", err)
	}
	job := toPublishJob(row)
	return &job, nil
}

// ListDuePublishJobs é o backstop do agendador ETA.
func (r *Repository) ListDuePublishJobs(ctx context.Context, limit int32) ([]PublishJob, error) {
	rows, err := r.queries.ListDueSessionPublishJobs(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("listing due publish jobs: %w", err)
	}
	out := make([]PublishJob, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPublishJob(row))
	}
	return out, nil
}

// ListStuckPublishJobs acha o job preso em 'publishing' porque o processo
// morreu entre a reivindicação e o desfecho.
func (r *Repository) ListStuckPublishJobs(ctx context.Context, olderThan time.Time, limit int32) ([]PublishJob, error) {
	rows, err := r.queries.ListStuckSessionPublishJobs(ctx, sqlc.ListStuckSessionPublishJobsParams{
		LastAttemptAt: pgtype.Timestamptz{Time: olderThan, Valid: true},
		Limit:         limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing stuck publish jobs: %w", err)
	}
	out := make([]PublishJob, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPublishJob(row))
	}
	return out, nil
}

func toPublishJob(row sqlc.SessionPublishJob) PublishJob {
	job := PublishJob{
		ID:               uuidToString(row.ID),
		StoreID:          uuidToString(row.StoreID),
		SessionID:        uuidToString(row.SessionID),
		EventID:          uuidToString(row.EventID),
		MediaKind:        row.MediaKind,
		AssetPath:        row.AssetPath,
		AssetContentType: row.AssetContentType,
		Caption:          row.Caption,
		Title:            row.Title,
		ScheduledFor:     row.ScheduledFor.Time,
		Status:           row.Status,
		PublishedMediaID: row.PublishedMediaID.String,
		Attempts:         int(row.Attempts),
		LastError:        row.LastError.String,
		CreatedAt:        row.CreatedAt.Time,
	}
	job.ProductIDs = make([]string, 0, len(row.ProductIds))
	for _, p := range row.ProductIds {
		job.ProductIDs = append(job.ProductIDs, uuidToString(p))
	}
	job.StartsAt = timestamptzToPtr(row.StartsAt)
	job.EndsAt = timestamptzToPtr(row.EndsAt)
	if row.CartExpirationMinutes.Valid {
		v := int(row.CartExpirationMinutes.Int32)
		job.CartExpirationMinutes = &v
	}
	if row.CartMaxQuantityPerItem.Valid {
		v := int(row.CartMaxQuantityPerItem.Int32)
		job.CartMaxQuantityPerItem = &v
	}
	job.PublishedAt = timestamptzToPtr(row.PublishedAt)
	job.CancelledAt = timestamptzToPtr(row.CancelledAt)
	return job
}

// optionalTimestamptz / optionalInt4 traduzem "não informado" (nil) para NULL.
// Vivem aqui porque o inverso (timestamptzToPtr) já existe em repository.go e
// não havia par de ida.
func optionalTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func optionalInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

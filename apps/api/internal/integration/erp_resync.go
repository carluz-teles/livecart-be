package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
)

const erpResyncBatchSize = 5

// ERPResyncProgress survives a deployment and is independent of provider metadata.
type ERPResyncProgress struct {
	RunID         string     `json:"runId"`
	Status        string     `json:"status"`
	Total         int        `json:"total"`
	Done          int        `json:"done"`
	Succeeded     int        `json:"succeeded"`
	Failed        int        `json:"failed"`
	StartedAt     time.Time  `json:"startedAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	NextAttemptAt time.Time  `json:"nextAttemptAt"`
}

func (p *ERPResyncProgress) Running() bool {
	return p != nil && (p.Status == "queued" || p.Status == "running" || p.Status == "retrying")
}

type ERPResyncCommand struct {
	StoreID       string `json:"store_id"`
	IntegrationID string `json:"integration_id"`
	RunID         string `json:"run_id"`
	DispatchID    string `json:"dispatch_id"`
}

type erpResyncJob struct {
	ERPResyncProgress
	command ERPResyncCommand
	items   []string
	owner   string
	retries int
}

const resyncColumns = `run_id::text,status,total,processed,succeeded,failed,created_at,updated_at,finished_at,next_attempt_at`

func scanResyncProgress(row pgx.Row) (*ERPResyncProgress, error) {
	p := &ERPResyncProgress{}
	err := row.Scan(&p.RunID, &p.Status, &p.Total, &p.Done, &p.Succeeded, &p.Failed,
		&p.StartedAt, &p.UpdatedAt, &p.FinishedAt, &p.NextAttemptAt)
	return p, err
}

// StartERPResync commits the checkpoint and delivery command together. A fresh
// run has a fresh task ID; an archived task from a previous run cannot block it.
func (s *Service) StartERPResync(ctx context.Context, input StartERPResyncInput) (*ERPResyncProgress, error) {
	integration, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}
	if integration.Type != "erp" || integration.Status != "active" {
		return nil, httpx.ErrUnprocessable("conecte uma integração de ERP ativa para sincronizar os produtos")
	}
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	// The parent lock also serializes the first run, before a job row exists.
	if _, err = tx.Exec(ctx, `SELECT id FROM integrations WHERE id=$1 FOR UPDATE`, integration.ID); err != nil {
		return nil, err
	}
	current, err := scanResyncProgress(tx.QueryRow(ctx,
		`SELECT `+resyncColumns+` FROM erp_resync_jobs WHERE integration_id=$1`, integration.ID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil && current.Running() {
		return current, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT external_id FROM products WHERE store_id=$1 AND external_source=$2
		AND external_id IS NOT NULL AND external_id<>'' ORDER BY id`, input.StoreID, integration.Provider)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return &ERPResyncProgress{Status: "idle"}, nil
	}
	command := ERPResyncCommand{StoreID: input.StoreID, IntegrationID: integration.ID,
		RunID: uuid.NewString(), DispatchID: uuid.NewString()}
	progress, err := scanResyncProgress(tx.QueryRow(ctx, `INSERT INTO erp_resync_jobs
		(integration_id,run_id,dispatch_id,status,product_ids,total) VALUES($1,$2,$3,'queued',$4,$5)
		ON CONFLICT(integration_id) DO UPDATE SET run_id=EXCLUDED.run_id,dispatch_id=EXCLUDED.dispatch_id,
		status='queued',product_ids=EXCLUDED.product_ids,total=EXCLUDED.total,processed=0,succeeded=0,failed=0,
		consecutive_retries=0,last_error=NULL,lease_owner=NULL,lease_until=NULL,
		next_attempt_at=now(),created_at=now(),updated_at=now(),finished_at=NULL RETURNING `+resyncColumns,
		integration.ID, command.RunID, command.DispatchID, ids, len(ids)))
	if err != nil {
		return nil, err
	}
	if err = emitERPResync(ctx, tx, command); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	logger.From(ctx, s.logger).Info("ERP resync queued", zap.String("store_id", input.StoreID),
		zap.String("integration_id", integration.ID), zap.String("run_id", command.RunID), zap.Int("total", len(ids)))
	return progress, nil
}

func emitERPResync(ctx context.Context, tx pgx.Tx, command ERPResyncCommand) error {
	return events.EmitInternal(ctx, sqlc.New(tx), events.ERPResyncProducts,
		"erp-resync:"+uuid.NewString(), command)
}

func (r *Repository) resyncProgressForStore(ctx context.Context, storeID string) (map[string]*ERPResyncProgress, error) {
	rows, err := r.pool.Query(ctx, `SELECT j.integration_id::text,j.run_id::text,j.status,j.total,j.processed,
		j.succeeded,j.failed,j.created_at,j.updated_at,j.finished_at,j.next_attempt_at
		FROM erp_resync_jobs j JOIN integrations i ON i.id=j.integration_id WHERE i.store_id=$1`, storeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]*ERPResyncProgress)
	for rows.Next() {
		var id string
		p := &ERPResyncProgress{}
		if err := rows.Scan(&id, &p.RunID, &p.Status, &p.Total, &p.Done, &p.Succeeded, &p.Failed,
			&p.StartedAt, &p.UpdatedAt, &p.FinishedAt, &p.NextAttemptAt); err != nil {
			return nil, err
		}
		result[id] = p
	}
	return result, rows.Err()
}

func (r *Repository) claimERPResync(ctx context.Context, command ERPResyncCommand) (*erpResyncJob, error) {
	job := &erpResyncJob{command: command, owner: uuid.NewString()}
	err := r.pool.QueryRow(ctx, `UPDATE erp_resync_jobs SET status='running',lease_owner=$4,
		lease_until=now()+interval '3 minutes',updated_at=now()
		WHERE integration_id=$1 AND run_id=$2 AND dispatch_id=$3
		AND status IN ('queued','retrying') AND next_attempt_at<=now()
		RETURNING product_ids,total,processed,succeeded,failed,consecutive_retries,created_at`,
		command.IntegrationID, command.RunID, command.DispatchID, job.owner).Scan(
		&job.items, &job.Total, &job.Done, &job.Succeeded, &job.Failed, &job.retries, &job.StartedAt)
	return job, err
}

var errERPResyncLeaseLost = errors.New("ERP resync execution was superseded")

// Advance only after the product write succeeds (or a definitive product error).
// Retrying a crash before this checkpoint repeats an idempotent read/sync only.
func (r *Repository) advanceERPResync(ctx context.Context, job *erpResyncJob, productErr error) error {
	succeeded, failed := 1, 0
	var message any
	if productErr != nil {
		succeeded, failed = 0, 1
		message = resyncErrorText(productErr)
	}
	tag, err := r.pool.Exec(ctx, `UPDATE erp_resync_jobs SET processed=processed+1,succeeded=succeeded+$4,
		failed=failed+$5,last_error=$6,consecutive_retries=0,updated_at=now(),lease_until=now()+interval '3 minutes'
		WHERE integration_id=$1 AND run_id=$2 AND lease_owner=$3 AND status='running' AND processed=$7`,
		job.command.IntegrationID, job.command.RunID, job.owner, succeeded, failed, message, job.Done)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errERPResyncLeaseLost
	}
	job.Done++
	job.Succeeded += succeeded
	job.Failed += failed
	job.retries = 0
	return nil
}

func resyncErrorText(err error) string {
	return string([]rune(err.Error())[:min(500, len([]rune(err.Error())))])
}

func (r *Repository) releaseERPResync(ctx context.Context, job *erpResyncJob, cause error) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	status := "queued"
	var message any
	delay := time.Duration(0)
	if cause != nil {
		status = "retrying"
		message = resyncErrorText(cause)
		delay = min(15*time.Minute, 30*time.Second*time.Duration(1<<min(job.retries, 5)))
		var limited *ratelimit.ErrRateLimited
		if errors.As(cause, &limited) {
			delay = max(delay, limited.RetryAfter)
		}
		if time.Since(job.StartedAt) >= 72*time.Hour {
			status = "failed"
		}
	}
	finished := job.Done == job.Total
	if finished {
		status = "completed"
		if job.Failed > 0 {
			status = "completed_with_errors"
		}
	}
	command := job.command
	command.DispatchID = uuid.NewString()
	tag, err := tx.Exec(ctx, `UPDATE erp_resync_jobs SET status=$4,dispatch_id=$5,last_error=COALESCE($6,last_error),
		lease_owner=NULL,lease_until=NULL,updated_at=now(),next_attempt_at=now()+make_interval(secs=>$7),
		consecutive_retries=CASE WHEN $6::text IS NULL THEN 0 ELSE consecutive_retries+1 END,
		finished_at=CASE WHEN $4 IN ('completed','completed_with_errors','failed') THEN now() ELSE NULL END,
		product_ids=CASE WHEN $4 IN ('completed','completed_with_errors') THEN '{}'::text[] ELSE product_ids END
		WHERE integration_id=$1 AND run_id=$2 AND lease_owner=$3 AND status='running'`,
		command.IntegrationID, command.RunID, job.owner, status, command.DispatchID, message, delay.Seconds())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, errERPResyncLeaseLost
	}
	if status == "queued" {
		if err = emitERPResync(ctx, tx, command); err != nil {
			return false, err
		}
	}
	return finished, tx.Commit(ctx)
}

func transientERPResyncError(err error) bool {
	var limited *ratelimit.ErrRateLimited
	var network net.Error
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errResyncStockDeferred) || errors.As(err, &limited) || errors.As(err, &network)
}

func (s *Service) RunERPResync(ctx context.Context, command ERPResyncCommand) error {
	// Legacy tasks have the fixed integration ID, no checkpoint and may have
	// survived a deployment. They must not start another untracked full scan.
	if command.RunID == "" || command.DispatchID == "" {
		return nil
	}
	job, err := s.repo.claimERPResync(ctx, command)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctx = ratelimit.WithTinyCatalogRead(ctx)
	integration, err := s.repo.GetByID(ctx, command.IntegrationID, command.StoreID)
	if err == nil && integration.Status != "active" {
		err = errors.New("ERP integration is not active")
	}
	if err == nil {
		err = s.runERPResyncBatch(ctx, integration, job)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	finished, finishErr := s.repo.releaseERPResync(finishCtx, job, err)
	if errors.Is(finishErr, errERPResyncLeaseLost) {
		return nil
	}
	if finishErr != nil {
		return fmt.Errorf("saving ERP resync checkpoint: %w", finishErr)
	}
	lg := logger.From(ctx, s.logger)
	lg.Info("ERP resync progress", zap.String("store_id", command.StoreID),
		zap.String("integration_id", command.IntegrationID), zap.String("run_id", command.RunID),
		zap.Int("processed", job.Done), zap.Int("total", job.Total), zap.Int("synced", job.Succeeded),
		zap.Int("failed", job.Failed), zap.Bool("finished", finished), zap.Error(err))
	if finished && integration != nil {
		coverage, coverageErr := s.repo.catalogIdentifierCoverage(finishCtx, command.StoreID, integration.Provider)
		if coverageErr == nil {
			lg.Info("ERP resync identifier coverage", zap.String("store_id", command.StoreID),
				zap.String("run_id", command.RunID), zap.Int("products", coverage.total),
				zap.Int("missing_sku", coverage.missingSKU), zap.Int("missing_barcode", coverage.missingBarcode))
		} else {
			lg.Warn("ERP resync identifier coverage unavailable", zap.String("run_id", command.RunID), zap.Error(coverageErr))
		}
	}
	if finished && s.erpResyncNotifier != nil && integration != nil {
		if err := s.erpResyncNotifier.NotifyERPResyncFinished(finishCtx, command.StoreID,
			integration.Provider, job.Succeeded, job.Failed); err != nil {
			lg.Warn("ERP resync notification failed", zap.String("run_id", command.RunID), zap.Error(err))
		}
	}
	return nil
}

func (s *Service) runERPResyncBatch(ctx context.Context, integration *IntegrationRow, job *erpResyncJob) error {
	end := min(job.Done+erpResyncBatchSize, job.Total)
	for job.Done < end {
		if err := ctx.Err(); err != nil {
			return err
		}
		externalID := job.items[job.Done]
		err := s.resyncOneProduct(ctx, integration, externalID)
		if transientERPResyncError(err) {
			return err
		}
		if err != nil {
			fields := []zap.Field{zap.String("run_id", job.command.RunID), zap.String("store_id", integration.StoreID),
				zap.String("integration_id", integration.ID), zap.String("external_product_id", externalID), zap.Error(err)}
			if errors.Is(err, errERPStockPendingEdit) {
				logger.From(ctx, s.logger).Info("ERP resync product awaits order edit reconciliation", fields...)
			} else {
				logger.From(ctx, s.logger).Warn("ERP resync product failed", fields...)
			}
		}
		if err := s.repo.advanceERPResync(ctx, job, err); err != nil {
			return err
		}
	}
	return nil
}

// Recover also covers a lost/archived Redis command. The database lease fences
// old workers; each redispatch has its own outbox/task ID, preserving the cursor.
// Queued work keeps its claim token so a queue backlog does not invalidate its
// existing place in the queue. Only a recovered execution receives a new token.
func (s *Service) RecoverERPResync(ctx context.Context) {
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		s.logger.Error("ERP resync recovery failed", zap.Error(err))
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT j.integration_id::text,i.store_id::text,j.run_id::text,
		CASE WHEN j.status='queued' THEN j.dispatch_id::text ELSE gen_random_uuid()::text END
		FROM erp_resync_jobs j JOIN integrations i ON i.id=j.integration_id
		WHERE (j.status='retrying' AND j.next_attempt_at<=now())
		OR (j.status='running' AND j.lease_until<now())
		OR (j.status='queued' AND j.updated_at<now()-interval '5 minutes')
		ORDER BY j.updated_at LIMIT 10 FOR UPDATE OF j SKIP LOCKED`)
	if err != nil {
		s.logger.Error("listing ERP resync recovery", zap.Error(err))
		return
	}
	commands := make([]ERPResyncCommand, 0)
	for rows.Next() {
		command := ERPResyncCommand{}
		if err := rows.Scan(&command.IntegrationID, &command.StoreID, &command.RunID, &command.DispatchID); err != nil {
			rows.Close()
			s.logger.Error("reading ERP resync recovery", zap.Error(err))
			return
		}
		commands = append(commands, command)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.logger.Error("reading ERP resync recovery", zap.Error(err))
		return
	}
	for _, command := range commands {
		_, err = tx.Exec(ctx, `UPDATE erp_resync_jobs SET status='queued',dispatch_id=$2,
			lease_owner=NULL,lease_until=NULL,next_attempt_at=now(),updated_at=now() WHERE integration_id=$1`,
			command.IntegrationID, command.DispatchID)
		if err == nil {
			err = emitERPResync(ctx, tx, command)
		}
		if err != nil {
			s.logger.Error("dispatching ERP resync recovery", zap.Error(err))
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		s.logger.Error("committing ERP resync recovery", zap.Error(err))
		return
	}
	if len(commands) > 0 {
		s.logger.Info("ERP resync recovery dispatched", zap.Int("runs", len(commands)))
	}
}

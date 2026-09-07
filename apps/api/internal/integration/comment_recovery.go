package integration

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livecart/apps/api/internal/live"
)

var _ live.CommentWorkRepository = (*Repository)(nil)

func (r *Repository) BeginCommentWork(ctx context.Context, commentID string, payload []byte) (string, bool, error) {
	// Existing comments predate the recovery protocol and must never be replayed
	// automatically: their local effects have no per-comment checkpoint.
	_, err := r.pool.Exec(ctx, `INSERT INTO live_comment_work(platform_comment_id,payload,completed_at)
        VALUES ($1,$2,CASE WHEN EXISTS(SELECT 1 FROM live_comments WHERE platform_comment_id=$1)
            THEN now() ELSE NULL END) ON CONFLICT DO NOTHING`, commentID, payload)
	if err != nil {
		return "", false, err
	}
	owner := uuid.NewString()
	var claimed string
	err = r.pool.QueryRow(ctx, `UPDATE live_comment_work SET lease_owner=$2,lease_until=now()+interval '3 minutes',attempts=attempts+1
        WHERE platform_comment_id=$1 AND completed_at IS NULL AND (lease_until IS NULL OR lease_until<now())
        RETURNING platform_comment_id`, commentID, owner).Scan(&claimed)
	if err == nil {
		return owner, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	var completed bool
	if err := r.pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM live_comment_work WHERE platform_comment_id=$1`, commentID).Scan(&completed); err != nil {
		return "", false, err
	}
	if completed {
		return "", true, nil
	}
	return "", false, live.ErrCommentBusy
}

func (r *Repository) FinishCommentWork(ctx context.Context, commentID, owner string, processingErr error) error {
	var message any
	if processingErr != nil {
		message = processingErr.Error()
	}
	_, err := r.pool.Exec(ctx, `WITH finished AS (UPDATE live_comment_work SET lease_owner=NULL,lease_until=NULL,last_error=$3,
		completed_at=CASE WHEN $3::text IS NULL OR ($4 AND accepted_at IS NULL AND created_at<now()-interval '24 hours') THEN now() ELSE NULL END,
        next_attempt_at=now()+make_interval(secs=>LEAST(900,5*power(2,LEAST(attempts,7)))::double precision)
        WHERE platform_comment_id=$1 AND lease_owner=$2 RETURNING platform_comment_id)
        UPDATE webhook_events SET processed=($3::text IS NULL),processed_at=CASE WHEN $3::text IS NULL THEN now() ELSE NULL END,error_message=$3
		WHERE provider='instagram' AND event_id IN (SELECT platform_comment_id FROM finished)`, commentID, owner, message, errors.Is(processingErr, live.ErrCommentMediaPending))
	return err
}

func (r *Repository) ListPendingCommentWork(ctx context.Context, limit int) ([][]byte, error) {
	rows, err := r.pool.Query(ctx, `SELECT payload FROM live_comment_work
        WHERE completed_at IS NULL AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now())
        ORDER BY next_attempt_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, rows.Err()
}

func (r *Repository) CommentWasAccepted(ctx context.Context, id string) (bool, error) {
	var accepted bool
	err := r.pool.QueryRow(ctx, `SELECT accepted_at IS NOT NULL FROM live_comment_work WHERE platform_comment_id=$1`, id).Scan(&accepted)
	return accepted, err
}
func (r *Repository) AcceptComment(ctx context.Context, id string, plan []byte) error {
	_, err := r.pool.Exec(ctx, `UPDATE live_comment_work SET accepted_at=now(),accepted_plan=$2 WHERE platform_comment_id=$1 AND accepted_at IS NULL`, id, plan)
	return err
}

func (r *Repository) AcceptedCommentPlan(ctx context.Context, id string) ([]byte, error) {
	var plan []byte
	err := r.pool.QueryRow(ctx, `SELECT accepted_plan FROM live_comment_work WHERE platform_comment_id=$1`, id).Scan(&plan)
	return plan, err
}

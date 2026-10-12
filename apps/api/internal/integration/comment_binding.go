package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/internal/live"
)

var _ live.CommentBindingRepository = (*Repository)(nil)

// Shared by resolution and purchase acceptance so a media/account change
// between those operations cannot attach a purchase to another business.
// Keep account matching consistent with ListActiveInstagramStoreIDsByAccount.
const awaitedCommentSession = `SELECT DISTINCT s.id
	FROM live_sessions s JOIN live_events e ON e.id=s.event_id
	JOIN live_session_platforms p ON p.session_id=s.id
	WHERE w.binding_wait_until>clock_timestamp()
	AND p.platform='instagram' AND p.platform_live_id=w.payload->>'MediaID' AND p.released_at IS NULL
	AND p.added_at<w.binding_wait_until AND s.type='live'
	AND e.store_id=ANY(w.binding_store_ids)
	AND (w.binding_session_id IS NULL OR w.binding_session_id=s.id)
	AND EXISTS (SELECT 1 FROM integrations i
		JOIN integrations source ON source.id=COALESCE(i.instagram_credentials_source_id,i.id)
		WHERE i.store_id=e.store_id AND i.type='social' AND i.provider='instagram' AND i.status='active'
		AND source.type='social' AND source.provider='instagram' AND source.status='active'
		AND source.instagram_credentials_source_id IS NULL
		AND (source.metadata->>'instagram_user_id'=w.payload->>'AccountID'
			OR source.metadata->>'instagram_app_scoped_id'=w.payload->>'AccountID'))
	AND NOT EXISTS (SELECT 1 FROM live_session_platforms other
		WHERE other.platform='instagram' AND other.platform_live_id=p.platform_live_id
		AND other.session_id<>s.id)`

func (r *Repository) LoadCommentBindingWork(ctx context.Context, commentID, owner string) (live.CommentBindingWork, error) {
	var work live.CommentBindingWork
	var payload []byte
	err := r.pool.QueryRow(ctx, `SELECT payload,binding_wait_until,COALESCE(binding_session_id::text,'')
		FROM live_comment_work WHERE platform_comment_id=$1 AND lease_owner=$2`, commentID, owner).
		Scan(&payload, &work.WaitUntil, &work.SessionID)
	if err != nil {
		return work, fmt.Errorf("loading comment binding work: %w", err)
	}
	if err := json.Unmarshal(payload, &work.Input); err != nil {
		return work, fmt.Errorf("decoding original comment: %w", err)
	}
	if work.Input.CommentID != commentID {
		return work, fmt.Errorf("original comment id does not match work")
	}
	return work, nil
}

func (r *Repository) AwaitCommentBinding(ctx context.Context, commentID, owner string) (bool, error) {
	work, err := r.LoadCommentBindingWork(ctx, commentID, owner)
	if err != nil {
		return false, err
	}
	input := work.Input
	if input.Timestamp <= 0 || input.AccountID == "" || input.MediaID == "" {
		return false, nil
	}
	ageSeconds := time.Now().Unix() - input.Timestamp
	if ageSeconds < 0 || ageSeconds >= int64(live.CommentBindingGracePeriod.Seconds()) {
		return false, nil
	}
	stores, err := r.ListInstagramStoreIDs(ctx, input.AccountID)
	if err != nil {
		return false, err
	}
	if len(stores) == 0 {
		return false, nil
	}
	// Both capture time and the platform's original timestamp bound recovery.
	// Redeliveries cannot renew the deadline or add newly connected stores.
	var waiting bool
	err = r.pool.QueryRow(ctx, `UPDATE live_comment_work SET
		binding_wait_until=COALESCE(binding_wait_until,
			LEAST(created_at,to_timestamp($4::double precision))+make_interval(secs=>$5)),
		binding_store_ids=COALESCE(binding_store_ids,$3::uuid[])
		WHERE platform_comment_id=$1 AND lease_owner=$2 AND accepted_at IS NULL
		AND to_timestamp($4::double precision)<=clock_timestamp()
		RETURNING binding_wait_until>clock_timestamp()`,
		commentID, owner, stores, input.Timestamp, live.CommentBindingGracePeriod.Seconds()).Scan(&waiting)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("awaiting comment binding: %w", err)
	}
	return waiting, nil
}

func (r *Repository) ResolveCommentBinding(ctx context.Context, commentID, owner string) (string, error) {
	work, err := r.LoadCommentBindingWork(ctx, commentID, owner)
	if err != nil {
		return "", err
	}
	if work.WaitUntil == nil || !time.Now().Before(*work.WaitUntil) {
		return "", nil
	}
	// Exact media only, with one historical session owner. Reused media must
	// not redirect a pending purchase into a different campaign or store.
	// The account must belong to the store both at capture and at resolution.
	var sessionID string
	err = r.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT w.platform_comment_id,(`+awaitedCommentSession+`) AS session_id
		FROM live_comment_work w WHERE w.platform_comment_id=$1 AND w.lease_owner=$2 AND w.accepted_at IS NULL
	) UPDATE live_comment_work w SET binding_session_id=candidate.session_id FROM candidate
		WHERE w.platform_comment_id=candidate.platform_comment_id AND candidate.session_id IS NOT NULL
		AND w.binding_wait_until>clock_timestamp()
		RETURNING w.binding_session_id::text`, commentID, owner).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolving awaited comment binding: %w", err)
	}
	return sessionID, nil
}

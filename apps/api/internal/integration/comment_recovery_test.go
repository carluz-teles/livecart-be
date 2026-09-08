//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/database"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/db/sqlc"
)

func TestCommentWork_ProductionPoolPreservesJSON(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	pool, err := database.NewPool(ctx, testPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := NewRepository(sqlc.New(pool), pool)
	id := fmt.Sprintf("production-json-%d", time.Now().UnixNano())
	wantText := "Eu quero \"1234\" 💜\nsegunda linha"
	payload, err := json.Marshal(map[string]string{"CommentID": id, "Text": wantText})
	if err != nil {
		t.Fatal(err)
	}
	owner, done, err := repo.BeginCommentWork(ctx, id, payload)
	if err != nil || done || owner == "" {
		t.Fatalf("claim with production connection: owner=%q completed=%v err=%v", owner, done, err)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM live_comment_work WHERE platform_comment_id=$1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(stored, &decoded); err != nil || decoded["CommentID"] != id || decoded["Text"] != wantText {
		t.Fatalf("comment payload was not preserved: %s err=%v", stored, err)
	}
	plan := []byte(`{"items":[{"sku":"1234","quantity":2}]}`)
	if err := repo.AcceptComment(ctx, id, plan); err != nil {
		t.Fatalf("accept with production connection: %v", err)
	}
	accepted, err := repo.CommentWasAccepted(ctx, id)
	if err != nil || !accepted {
		t.Fatalf("accepted plan missing: accepted=%v err=%v", accepted, err)
	}
	storedPlan, err := repo.AcceptedCommentPlan(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Items []struct {
			SKU      string `json:"sku"`
			Quantity int    `json:"quantity"`
		} `json:"items"`
	}
	if err := json.Unmarshal(storedPlan, &snapshot); err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].SKU != "1234" || snapshot.Items[0].Quantity != 2 {
		t.Fatalf("accepted plan was not preserved: %s err=%v", storedPlan, err)
	}
	if err := repo.FinishCommentWork(ctx, id, owner, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCommentWork_RetrySurvivesLeaseAndFailure(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	id := fmt.Sprintf("recover-%d", time.Now().UnixNano())
	owner, done, err := testRepo.BeginCommentWork(ctx, id, []byte(`{"CommentID":"test"}`))
	if err != nil || done || owner == "" {
		t.Fatalf("claim: %s %v %v", owner, done, err)
	}
	if _, _, err := testRepo.BeginCommentWork(ctx, id, []byte(`{}`)); !errors.Is(err, live.ErrCommentBusy) {
		t.Fatalf("concurrent claim: %v", err)
	}
	if err := testRepo.AcceptComment(ctx, id, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.FinishCommentWork(ctx, id, owner, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	owner, done, err = testRepo.BeginCommentWork(ctx, id, []byte(`{}`))
	if err != nil || done {
		t.Fatalf("retry claim: %v %v", done, err)
	}
	accepted, err := testRepo.CommentWasAccepted(ctx, id)
	if err != nil || !accepted {
		t.Fatalf("accepted snapshot: %v %v", accepted, err)
	}
	if err := testRepo.FinishCommentWork(ctx, id, owner, nil); err != nil {
		t.Fatal(err)
	}
	_, done, err = testRepo.BeginCommentWork(ctx, id, []byte(`{}`))
	if err != nil || !done {
		t.Fatalf("completed claim: %v %v", done, err)
	}
}

func TestCommentWork_UnknownMediaIsIgnored(t *testing.T) {
	requireDB(t)
	for _, name := range []string{"new comment", "previously pending comment"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			id := fmt.Sprintf("ignored-%d", time.Now().UnixNano())
			input := live.ProcessInstagramCommentInput{
				CommentID: id,
				MediaID:   "unlinked-" + id,
				UserID:    "buyer-" + id,
				Text:      "Eu quero 1234",
			}
			svc := live.NewService(live.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
			svc.SetIngestRepository(liveIngestRepoAdapter{testRepo})
			if name == "previously pending comment" {
				payload, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				owner, _, err := testRepo.BeginCommentWork(ctx, id, payload)
				if err != nil {
					t.Fatal(err)
				}
				// Simulate work left pending by the previous deployment.
				if err := testRepo.FinishCommentWork(ctx, id, owner,
					errors.New("comment media has no session yet")); err != nil {
					t.Fatal(err)
				}
				if _, err := testPool.Exec(ctx, `UPDATE live_comment_work
					SET next_attempt_at='2000-01-01' WHERE platform_comment_id=$1`, id); err != nil {
					t.Fatal(err)
				}
				svc.RecoverPendingComments(ctx)
			} else if err := svc.ProcessInstagramComment(ctx, input); err != nil {
				t.Fatalf("ignore unlinked media: %v", err)
			}

			var done, accepted, hasError bool
			var comments, items, attempts int
			if err := testPool.QueryRow(ctx, `SELECT completed_at IS NOT NULL,
				accepted_at IS NOT NULL, last_error IS NOT NULL, attempts,
				(SELECT count(*) FROM live_comments WHERE platform_comment_id=$1),
				(SELECT count(*) FROM cart_item_events WHERE platform_comment_id=$1)
				FROM live_comment_work WHERE platform_comment_id=$1`, id).
				Scan(&done, &accepted, &hasError, &attempts, &comments, &items); err != nil {
				t.Fatal(err)
			}
			if !done || accepted || hasError || comments != 0 || items != 0 {
				t.Fatalf("done=%v accepted=%v error=%v comments=%d items=%d",
					done, accepted, hasError, comments, items)
			}
			// A duplicate webhook must not start processing this comment again.
			if err := svc.ProcessInstagramComment(ctx, input); err != nil {
				t.Fatal(err)
			}
			var after int
			if err := testPool.QueryRow(ctx,
				`SELECT attempts FROM live_comment_work WHERE platform_comment_id=$1`, id).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != attempts {
				t.Fatalf("ignored comment retried: attempts before=%d after=%d", attempts, after)
			}
		})
	}
}

var _ live.CommentWorkRepository = liveIngestRepoAdapter{}

type recoveringCommentERP struct {
	fail  bool
	calls int
}

type recoveringCommentMessenger struct {
	fail  bool
	calls int
}

func (m *recoveringCommentMessenger) SendInstagramDM(context.Context, string, string, string) error {
	m.calls++
	if m.fail {
		return context.DeadlineExceeded
	}
	return nil
}
func (m *recoveringCommentMessenger) ReplyToInstagramComment(ctx context.Context, store, comment, text string) error {
	return m.SendInstagramDM(ctx, store, comment, text)
}

func (r *recoveringCommentERP) NoteReserved(context.Context, live.ReserveParams) error { return nil }
func (r *recoveringCommentERP) ReserveStockInERP(context.Context, string, string, string, string, int, int64, string) error {
	r.calls++
	if r.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func TestCommentWork_FullPipelineRetriesAfterERPFailureAndSessionEnd(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 10, 0)
	if _, err := testPool.Exec(ctx, `UPDATE products SET keyword='1234' WHERE id=$1`, productID); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := testPool.QueryRow(ctx, `INSERT INTO live_sessions(event_id,status,type,sequence_order) VALUES($1,'live','live',1) RETURNING id::text`, fx.eventID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	media := "media-" + sessionID
	if _, err := testPool.Exec(ctx, `INSERT INTO live_session_platforms(session_id,platform,platform_live_id) VALUES($1,'instagram',$2)`, sessionID, media); err != nil {
		t.Fatal(err)
	}
	svc := live.NewService(live.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
	svc.SetIngestRepository(liveIngestRepoAdapter{testRepo})
	if _, err := testPool.Exec(ctx, `UPDATE stores SET cart_real_time=true WHERE id=$1`, fx.storeID); err != nil {
		t.Fatal(err)
	}
	messenger := &recoveringCommentMessenger{fail: true}
	svc.SetNotificationService(notification.NewService(sqlc.New(testPool), messenger, zap.NewNop()))
	erp := &recoveringCommentERP{fail: true}
	svc.SetStockReserver(erp)
	input := live.ProcessInstagramCommentInput{MediaID: media, CommentID: "comment-" + sessionID, UserID: "buyer", Username: "buyer", Text: "Eu quero 1234 x2", Timestamp: time.Now().Unix()}
	if err := svc.ProcessInstagramComment(ctx, input); !errors.Is(err, live.ErrCommentERPPending) {
		t.Fatalf("first attempt: %v", err)
	}
	if messenger.calls != 0 {
		t.Fatal("buyer was notified before ERP confirmation")
	}
	if _, err := testPool.Exec(ctx, `UPDATE live_sessions SET status='ended',processing_paused=true WHERE id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE live_session_platforms SET platform_live_id=platform_live_id||'-released' WHERE session_id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}
	erp.fail = false
	if err := svc.ProcessInstagramComment(ctx, input); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("notification failure should remain recoverable: %v", err)
	}
	messenger.fail = false
	if err := svc.ProcessInstagramComment(ctx, input); err != nil {
		t.Fatal(err)
	}
	// Losing the job acknowledgement after a confirmed send must not resend it.
	if _, err := testPool.Exec(ctx, `UPDATE live_comment_work SET completed_at=NULL WHERE platform_comment_id=$1`, input.CommentID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessInstagramComment(ctx, input); err != nil {
		t.Fatal(err)
	}
	var stock, quantity, checkpoints, comments int
	var done bool
	if err := testPool.QueryRow(ctx, `SELECT p.stock,(SELECT SUM(quantity) FROM cart_items WHERE product_id=p.id),(SELECT COUNT(*) FROM cart_item_events WHERE platform_comment_id=$2),(SELECT total_comments FROM live_sessions WHERE id=$3),(SELECT completed_at IS NOT NULL FROM live_comment_work WHERE platform_comment_id=$2) FROM products p WHERE p.id=$1`, productID, input.CommentID, sessionID).Scan(&stock, &quantity, &checkpoints, &comments, &done); err != nil {
		t.Fatal(err)
	}
	if stock != 8 || quantity != 2 || checkpoints != 1 || comments != 1 || !done || erp.calls != 4 || messenger.calls != 3 {
		t.Fatalf("stock=%d quantity=%d checkpoints=%d comments=%d done=%v ERP=%d", stock, quantity, checkpoints, comments, done, erp.calls)
	}
	var sent int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM notification_logs WHERE platform_comment_id=$1 AND status='sent'`, input.CommentID).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("successful buyer messages=%d", sent)
	}
}

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/live"
)

type commentBindingFixture struct {
	scaleFixture
	service   *live.Service
	input     live.ProcessInstagramCommentInput
	sessionID string
	productID string
	erp       *recoveringCommentERP
}

func seedAwaitingComment(t *testing.T) commentBindingFixture {
	t.Helper()
	requireDB(t)
	fx := commentBindingFixture{scaleFixture: seedScaleEvent(t)}
	account := "binding-account-" + fx.storeID
	integrationID := seedSharedInstagram(t, fx.storeID, account, "scoped-"+account, "active")
	bindingExec(t, `UPDATE integrations SET token_expires_at=now()+interval '1 hour' WHERE id=$1`, integrationID)
	fx.productID = seedSoldOutProductWithQueue(t, fx.scaleFixture, 10, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET keyword='1234' WHERE id=$1`, fx.productID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `INSERT INTO live_sessions(event_id,status,type,sequence_order)
		VALUES($1,'live','live',1) RETURNING id::text`, fx.eventID).Scan(&fx.sessionID); err != nil {
		t.Fatal(err)
	}
	fx.input = live.ProcessInstagramCommentInput{
		AccountID: account, MediaID: "binding-media-" + fx.sessionID,
		CommentID: "binding-comment-" + fx.sessionID, UserID: "buyer", Username: "buyer",
		Text: "Eu quero 1234", Timestamp: time.Now().Add(-30 * time.Second).Unix(),
	}
	fx.service = live.NewService(live.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
	fx.service.SetIngestRepository(liveIngestRepoAdapter{testRepo})
	fx.erp = &recoveringCommentERP{}
	fx.service.SetStockReserver(fx.erp)
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM live_comment_work WHERE platform_comment_id=$1`, fx.input.CommentID); err != nil {
			t.Error(err)
		}
	})
	return fx
}

func (f commentBindingFixture) await(t *testing.T) {
	t.Helper()
	if err := f.service.ProcessInstagramCommentDelivery(t.Context(), f.input); err != nil {
		t.Fatalf("awaiting binding must acknowledge the durable delivery: %v", err)
	}
	var waiting bool
	if err := testPool.QueryRow(t.Context(), `SELECT completed_at IS NULL AND accepted_at IS NULL
		AND lease_owner IS NULL AND binding_wait_until>now() AND last_error=$2
		FROM live_comment_work WHERE platform_comment_id=$1`,
		f.input.CommentID, live.ErrCommentAwaitingBinding.Error()).Scan(&waiting); err != nil || !waiting {
		t.Fatalf("comment was not durably awaiting binding: waiting=%v err=%v", waiting, err)
	}
}

func (f commentBindingFixture) bind(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), `INSERT INTO live_session_platforms(session_id,platform,platform_live_id)
		VALUES($1,'instagram',$2)`, f.sessionID, f.input.MediaID); err != nil {
		t.Fatal(err)
	}
}

func (f commentBindingFixture) expireGrace(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), `UPDATE live_comment_work
		SET binding_wait_until=now()-interval '1 second',next_attempt_at=now() WHERE platform_comment_id=$1`,
		f.input.CommentID); err != nil {
		t.Fatal(err)
	}
}

func (f commentBindingFixture) assertNoPurchase(t *testing.T) {
	t.Helper()
	var accepted bool
	var items int
	if err := testPool.QueryRow(t.Context(), `SELECT accepted_at IS NOT NULL,
		(SELECT count(*) FROM cart_item_events WHERE platform_comment_id=$1)
		FROM live_comment_work WHERE platform_comment_id=$1`, f.input.CommentID).
		Scan(&accepted, &items); err != nil {
		t.Fatal(err)
	}
	if accepted || items != 0 || f.erp.calls != 0 {
		t.Fatalf("unexpected purchase: accepted=%v items=%d ERP calls=%d", accepted, items, f.erp.calls)
	}
}

func TestCommentBindingRecoversSinglePurchaseWithOriginalDelivery(t *testing.T) {
	fx := seedAwaitingComment(t)
	fx.await(t)
	fx.bind(t)
	makeCommentRetryDue(t, fx.input.CommentID)
	// A duplicate transport must not replace identity, quantity or timestamp.
	duplicate := fx.input
	duplicate.AccountID, duplicate.UserID = "unrelated-account", "other-buyer"
	duplicate.Text, duplicate.Timestamp = "Eu quero 1234 x9", time.Now().Unix()
	if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), duplicate); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), fx.input); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	fx.service.RecoverPendingComments(t.Context())
	var stock, quantity, checkpoints, comments int
	var done bool
	var buyer string
	var createdAt time.Time
	err := testPool.QueryRow(t.Context(), `SELECT p.stock,ci.quantity,c.platform_user_id,
		(SELECT count(*) FROM cart_item_events WHERE platform_comment_id=$2),s.total_comments,
		lc.created_at,w.completed_at IS NOT NULL
		FROM products p JOIN cart_items ci ON ci.product_id=p.id JOIN carts c ON c.id=ci.cart_id
		JOIN live_sessions s ON s.id=$3 JOIN live_comments lc ON lc.platform_comment_id=$2
		JOIN live_comment_work w ON w.platform_comment_id=$2 WHERE p.id=$1`,
		fx.productID, fx.input.CommentID, fx.sessionID).
		Scan(&stock, &quantity, &buyer, &checkpoints, &comments, &createdAt, &done)
	if err != nil {
		t.Fatal(err)
	}
	if stock != 9 || quantity != 1 || buyer != fx.input.UserID || checkpoints != 1 || comments != 1 || !done || fx.erp.calls != 1 {
		t.Fatalf("stock=%d quantity=%d buyer=%q checkpoints=%d comments=%d done=%v ERP=%d",
			stock, quantity, buyer, checkpoints, comments, done, fx.erp.calls)
	}
	if !createdAt.Equal(time.Unix(fx.input.Timestamp, 0)) {
		t.Fatalf("original timestamp changed: got=%v want=%v", createdAt, time.Unix(fx.input.Timestamp, 0))
	}
}

func TestCommentBindingRecoveryWorkerRetainsGreetings(t *testing.T) {
	fx := seedAwaitingComment(t)
	fx.input.Text = "boa noite"
	fx.await(t)
	fx.bind(t)
	makeCommentRetryDue(t, fx.input.CommentID)
	fx.service.RecoverPendingComments(t.Context())
	var result string
	if err := testPool.QueryRow(t.Context(), `SELECT result FROM live_comments WHERE platform_comment_id=$1`,
		fx.input.CommentID).Scan(&result); err != nil || result != "no_intent" {
		t.Fatalf("greeting was not recovered: result=%q err=%v", result, err)
	}
	fx.assertNoPurchase(t)
}

func TestCommentBindingCannotUseAProductHighlightedAfterTheComment(t *testing.T) {
	fx := seedAwaitingComment(t)
	fx.input.Text = "eu quero"
	fx.await(t)
	fx.bind(t)
	bindingExec(t, `UPDATE live_sessions SET current_active_product_id=$2 WHERE id=$1`, fx.sessionID, fx.productID)
	makeCommentRetryDue(t, fx.input.CommentID)
	fx.service.RecoverPendingComments(t.Context())
	fx.assertNoPurchase(t)
	var result string
	if err := testPool.QueryRow(t.Context(), `SELECT result FROM live_comments WHERE platform_comment_id=$1`,
		fx.input.CommentID).Scan(&result); err != nil || result != "no_product" {
		t.Fatalf("ambiguous product should be retained without a purchase: result=%q err=%v", result, err)
	}
}

func TestCommentBindingSharedAccountKeepsPurchasesInTheirOriginalStores(t *testing.T) {
	a, b := seedAwaitingComment(t), seedAwaitingComment(t)
	bindingExec(t, `DELETE FROM integrations WHERE store_id=$1 AND provider='instagram'`, b.storeID)
	var source string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1 AND provider='instagram'`,
		a.storeID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if err := linkInstagramFixture(t, source, b.storeID, a.input.AccountID); err != nil {
		t.Fatal(err)
	}
	b.input.AccountID = a.input.AccountID
	for _, fx := range []commentBindingFixture{a, b} {
		fx.await(t)
		fx.bind(t)
		makeCommentRetryDue(t, fx.input.CommentID)
	}
	a.service.RecoverPendingComments(t.Context())
	for _, fx := range []commentBindingFixture{a, b} {
		var store, product, session string
		var quantity int
		err := testPool.QueryRow(t.Context(), `SELECT c.store_id::text,ci.product_id::text,
			e.session_id::text,ci.quantity FROM cart_item_events e JOIN carts c ON c.id=e.cart_id
			JOIN cart_items ci ON ci.cart_id=c.id AND ci.product_id=e.product_id
			WHERE e.platform_comment_id=$1`, fx.input.CommentID).Scan(&store, &product, &session, &quantity)
		if err != nil || store != fx.storeID || product != fx.productID || session != fx.sessionID || quantity != 1 {
			t.Fatalf("shared account mixed stores: store=%s product=%s session=%s quantity=%d err=%v",
				store, product, session, quantity, err)
		}
	}
}

func TestCommentBindingGraceExpiresEvenWhenMediaBindsBeforeRetry(t *testing.T) {
	for _, bind := range []bool{false, true} {
		name := "unrelated media"
		if bind {
			name = "binding appears after grace"
		}
		t.Run(name, func(t *testing.T) {
			fx := seedAwaitingComment(t)
			fx.await(t)
			fx.expireGrace(t)
			if bind {
				fx.bind(t)
			}
			fx.service.RecoverPendingComments(t.Context())
			if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), fx.input); err != nil {
				t.Fatal(err)
			}
			fx.assertNoPurchase(t)
			var done bool
			var comments int
			if err := testPool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL,
				(SELECT count(*) FROM live_comments WHERE platform_comment_id=$1)
				FROM live_comment_work WHERE platform_comment_id=$1`, fx.input.CommentID).
				Scan(&done, &comments); err != nil || !done || comments != 0 {
				t.Fatalf("expired grace not terminal: done=%v comments=%d err=%v", done, comments, err)
			}
		})
	}
}

func TestCommentBindingRejectsUnsafeRecovery(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, commentBindingFixture)
	}{
		{"wrong store", func(t *testing.T, fx commentBindingFixture) {
			other := seedScaleEvent(t)
			bindingExec(t, `UPDATE live_sessions SET event_id=$2 WHERE id=$1`, fx.sessionID, other.eventID)
		}},
		{"account disconnected", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE integrations SET status='disconnected' WHERE store_id=$1 AND provider='instagram'`, fx.storeID)
		}},
		{"new store connection after capture", func(t *testing.T, fx commentBindingFixture) {
			other := seedScaleEvent(t)
			var source string
			if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1 AND provider='instagram'`, fx.storeID).Scan(&source); err != nil {
				t.Fatal(err)
			}
			if err := linkInstagramFixture(t, source, other.storeID, fx.input.AccountID); err != nil {
				t.Fatal(err)
			}
			bindingExec(t, `UPDATE live_sessions SET event_id=$2 WHERE id=$1`, fx.sessionID, other.eventID)
		}},
		{"unrelated post", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_sessions SET type='post' WHERE id=$1`, fx.sessionID)
		}},
		{"event ended", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_events SET status='ended' WHERE id=$1`, fx.eventID)
		}},
		{"event deadline expired", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_events SET ends_at=now()-interval '1 second' WHERE id=$1`, fx.eventID)
		}},
		{"session ended", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_sessions SET status='ended' WHERE id=$1`, fx.sessionID)
		}},
		{"original comment predates sales window", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_events SET scheduled_at=to_timestamp($2),starts_at=to_timestamp($2) WHERE id=$1`, fx.eventID, fx.input.Timestamp+10)
		}},
		{"media reused by another session", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_session_platforms SET released_at=now() WHERE session_id=$1`, fx.sessionID)
			var session string
			if err := testPool.QueryRow(t.Context(), `INSERT INTO live_sessions(event_id,status,type,sequence_order)
				VALUES($1,'live','live',2) RETURNING id::text`, fx.eventID).Scan(&session); err != nil {
				t.Fatal(err)
			}
			bindingExec(t, `INSERT INTO live_session_platforms(session_id,platform,platform_live_id)
				VALUES($1,'instagram',$2)`, session, fx.input.MediaID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedAwaitingComment(t)
			fx.await(t)
			fx.bind(t)
			tc.change(t, fx)
			makeCommentRetryDue(t, fx.input.CommentID)
			err := fx.service.ProcessInstagramComment(t.Context(), fx.input)
			if err != nil && !errors.Is(err, live.ErrCommentAwaitingBinding) {
				t.Fatal(err)
			}
			fx.assertNoPurchase(t)
		})
	}
}

func TestCommentBindingRechecksGraceAtPurchaseAcceptance(t *testing.T) {
	fx := seedAwaitingComment(t)
	fx.await(t)
	fx.bind(t)
	makeCommentRetryDue(t, fx.input.CommentID)
	payload, err := json.Marshal(fx.input)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := testRepo.BeginCommentWork(t.Context(), fx.input.CommentID, payload)
	if err != nil {
		t.Fatal(err)
	}
	session, err := testRepo.ResolveCommentBinding(t.Context(), fx.input.CommentID, owner)
	if err != nil || session != fx.sessionID {
		t.Fatalf("binding: %s %v", session, err)
	}
	fx.expireGrace(t)
	plan, err := json.Marshal(map[string]any{
		"session": map[string]string{"ID": fx.sessionID},
		"event":   map[string]string{"ID": fx.eventID, "StoreID": fx.storeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := testRepo.AcceptComment(t.Context(), fx.input.CommentID, plan); !errors.Is(err, live.ErrCommentAwaitingBinding) {
		t.Fatalf("purchase accepted after grace expired during processing: %v", err)
	}
	fx.assertNoPurchase(t)
}

func TestCommentBindingAcceptanceRechecksOwnershipAndWindows(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, commentBindingFixture)
		want   bool
	}{
		{"matching binding", func(*testing.T, commentBindingFixture) {}, true},
		{"disconnected account", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE integrations SET status='disconnected' WHERE store_id=$1`, fx.storeID)
		}, false},
		{"media released during lookup", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_session_platforms SET released_at=now() WHERE session_id=$1`, fx.sessionID)
		}, false},
		{"session ended during lookup", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_sessions SET status='ended' WHERE id=$1`, fx.sessionID)
		}, false},
		{"session paused during lookup", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_sessions SET processing_paused=true WHERE id=$1`, fx.sessionID)
		}, false},
		{"event expired during lookup", func(t *testing.T, fx commentBindingFixture) {
			bindingExec(t, `UPDATE live_events SET ends_at=now()-interval '1 second' WHERE id=$1`, fx.eventID)
		}, false},
		{"event moved during lookup", func(t *testing.T, fx commentBindingFixture) {
			other := seedScaleEvent(t)
			bindingExec(t, `UPDATE live_sessions SET event_id=$2 WHERE id=$1`, fx.sessionID, other.eventID)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedAwaitingComment(t)
			fx.await(t)
			fx.bind(t)
			makeCommentRetryDue(t, fx.input.CommentID)
			payload, err := json.Marshal(fx.input)
			if err != nil {
				t.Fatal(err)
			}
			owner, _, err := testRepo.BeginCommentWork(t.Context(), fx.input.CommentID, payload)
			if err != nil {
				t.Fatal(err)
			}
			if session, err := testRepo.ResolveCommentBinding(t.Context(), fx.input.CommentID, owner); err != nil || session != fx.sessionID {
				t.Fatalf("resolve: session=%s err=%v", session, err)
			}
			tc.change(t, fx)
			plan, err := json.Marshal(map[string]any{
				"session": map[string]string{"ID": fx.sessionID},
				"event":   map[string]string{"ID": fx.eventID, "StoreID": fx.storeID},
			})
			if err != nil {
				t.Fatal(err)
			}
			err = testRepo.AcceptComment(t.Context(), fx.input.CommentID, plan)
			if tc.want && err != nil {
				t.Fatal(err)
			}
			if !tc.want && !errors.Is(err, live.ErrCommentAwaitingBinding) {
				t.Fatalf("unsafe binding was accepted: %v", err)
			}
		})
	}
}

func TestCommentBindingAcceptedPurchaseFinishesAfterGraceAndSessionEnd(t *testing.T) {
	fx := seedAwaitingComment(t)
	fx.await(t)
	fx.bind(t)
	fx.erp.fail = true
	makeCommentRetryDue(t, fx.input.CommentID)
	if err := fx.service.ProcessInstagramComment(t.Context(), fx.input); !errors.Is(err, live.ErrCommentERPPending) {
		t.Fatalf("expected durable ERP pending work: %v", err)
	}
	fx.expireGrace(t)
	bindingExec(t, `UPDATE live_sessions SET status='ended' WHERE id=$1`, fx.sessionID)
	bindingExec(t, `UPDATE live_session_platforms SET platform_live_id=platform_live_id||'-released' WHERE session_id=$1`, fx.sessionID)
	fx.erp.fail = false
	fx.service.RecoverPendingComments(t.Context())
	var quantity, checkpoints int
	var done bool
	err := testPool.QueryRow(t.Context(), `SELECT ci.quantity,
		(SELECT count(*) FROM cart_item_events WHERE platform_comment_id=$2),w.completed_at IS NOT NULL
		FROM cart_items ci JOIN live_comment_work w ON w.platform_comment_id=$2
		WHERE ci.product_id=$1`, fx.productID, fx.input.CommentID).Scan(&quantity, &checkpoints, &done)
	if err != nil || quantity != 1 || checkpoints != 1 || !done || fx.erp.calls != 2 {
		t.Fatalf("accepted purchase lost/duplicated: quantity=%d checkpoints=%d done=%v calls=%d err=%v",
			quantity, checkpoints, done, fx.erp.calls, err)
	}
}

func TestCommentBindingDoesNotReopenCompletedHistoricalWork(t *testing.T) {
	fx := seedAwaitingComment(t)
	payload, err := json.Marshal(fx.input)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := testRepo.BeginCommentWork(t.Context(), fx.input.CommentID, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := testRepo.FinishCommentWork(t.Context(), fx.input.CommentID, owner, nil); err != nil {
		t.Fatal(err)
	}
	bindingExec(t, `UPDATE live_comment_work SET created_at=now()-interval '7 days' WHERE platform_comment_id=$1`, fx.input.CommentID)
	fx.bind(t)
	makeCommentRetryDue(t, fx.input.CommentID)
	fx.service.RecoverPendingComments(t.Context())
	if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), fx.input); err != nil {
		t.Fatal(err)
	}
	fx.assertNoPurchase(t)
}

func TestCommentBindingIgnoresUntrustedOrStaleUnknownMedia(t *testing.T) {
	cases := []struct {
		name   string
		change func(*live.ProcessInstagramCommentInput)
	}{
		{"unknown account", func(in *live.ProcessInstagramCommentInput) { in.AccountID = "unknown-account" }},
		{"missing timestamp", func(in *live.ProcessInstagramCommentInput) { in.Timestamp = 0 }},
		{"old comment", func(in *live.ProcessInstagramCommentInput) { in.Timestamp = time.Now().Add(-time.Hour).Unix() }},
		{"future timestamp", func(in *live.ProcessInstagramCommentInput) { in.Timestamp = time.Now().Add(time.Hour).Unix() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedAwaitingComment(t)
			tc.change(&fx.input)
			if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), fx.input); err != nil {
				t.Fatal(err)
			}
			var done bool
			if err := testPool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL FROM live_comment_work
				WHERE platform_comment_id=$1`, fx.input.CommentID).Scan(&done); err != nil || !done {
				t.Fatalf("invalid/stale input should not retry: done=%v err=%v", done, err)
			}
			fx.bind(t)
			if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), fx.input); err != nil {
				t.Fatal(err)
			}
			fx.assertNoPurchase(t)
		})
	}
}

func TestCommentBindingRetriesCannotExtendOriginalGrace(t *testing.T) {
	fx := seedAwaitingComment(t)
	fx.input.Timestamp = time.Now().Add(-4 * time.Minute).Unix()
	fx.await(t)
	var deadline time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT binding_wait_until FROM live_comment_work
		WHERE platform_comment_id=$1`, fx.input.CommentID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(fx.input.Timestamp, 0).Add(live.CommentBindingGracePeriod)
	if !deadline.Equal(want) {
		t.Fatalf("grace was extended beyond original comment timestamp: got=%v want=%v", deadline, want)
	}
	duplicate := fx.input
	duplicate.Timestamp = time.Now().Unix()
	for range 3 {
		makeCommentRetryDue(t, fx.input.CommentID)
		if err := fx.service.ProcessInstagramCommentDelivery(t.Context(), duplicate); err != nil {
			t.Fatal(err)
		}
	}
	var after, next time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT binding_wait_until,next_attempt_at
		FROM live_comment_work WHERE platform_comment_id=$1`, fx.input.CommentID).Scan(&after, &next); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(deadline) || next.After(deadline) {
		t.Fatalf("retry escaped bounded grace: before=%v after=%v retry=%v", deadline, after, next)
	}
	fx.assertNoPurchase(t)
}

type bindingFinishFailure struct {
	liveIngestRepoAdapter
	err error
}

func (r bindingFinishFailure) FinishCommentWork(context.Context, string, string, error) error {
	return r.err
}

func TestCommentBindingDeliveryDoesNotHidePersistenceFailure(t *testing.T) {
	fx := seedAwaitingComment(t)
	failure := errors.New("finish persistence unavailable")
	fx.service.SetIngestRepository(bindingFinishFailure{
		liveIngestRepoAdapter: liveIngestRepoAdapter{testRepo}, err: failure,
	})
	err := fx.service.ProcessInstagramCommentDelivery(t.Context(), fx.input)
	if !errors.Is(err, failure) || !errors.Is(err, live.ErrCommentAwaitingBinding) {
		t.Fatalf("delivery acknowledged an unpersisted retry: %v", err)
	}
}

func bindingExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

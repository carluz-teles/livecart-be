//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/user"
	"livecart/apps/api/lib/httpx"
)

type manualRecoveryFixture struct {
	commentBindingFixture
	commentID string
	cartID    string
	actorID   string
	clerkID   string
}

func seedManualRecovery(t *testing.T) manualRecoveryFixture {
	t.Helper()
	f := manualRecoveryFixture{commentBindingFixture: seedAwaitingComment(t)}
	f.bind(t)
	bindingExec(t, `UPDATE live_events SET created_at=now()-interval '1 hour',starts_at=now()-interval '1 hour' WHERE id=$1`, f.eventID)
	bindingExec(t, `UPDATE live_sessions SET created_at=now()-interval '1 hour',started_at=now()-interval '1 hour' WHERE id=$1`, f.sessionID)
	f.input.Text = "1923 X 1"
	f.input.Timestamp = time.Now().Add(-20 * time.Minute).Unix()
	if err := f.service.ProcessInstagramComment(t.Context(), f.input); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM live_comments WHERE platform_comment_id=$1`, f.input.CommentID).Scan(&f.commentID); err != nil {
		t.Fatal(err)
	}
	// The target incident: the cart was created by a later comment, and the
	// original live has ended while its parent event remains active.
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts(event_id,store_id,session_id,platform_user_id,platform_handle,token,status,payment_status,created_at,short_id)
		VALUES($1,$2,$3,$4,$5,$6,'active','pending',now()-interval '19 minutes',1446) RETURNING id::text`,
		f.eventID, f.storeID, f.sessionID, f.input.UserID, f.input.Username, uuid.NewString()).Scan(&f.cartID); err != nil {
		t.Fatal(err)
	}
	bindingExec(t, `UPDATE products SET keyword='1923' WHERE id=$1`, f.productID)
	bindingExec(t, `UPDATE live_sessions SET status='ended',ended_at=now()-interval '10 minutes' WHERE id=$1`, f.sessionID)
	member := seedStoreMember(t, f.storeID)
	if err := testPool.QueryRow(t.Context(), `SELECT m.id::text,u.clerk_id FROM memberships m JOIN users u ON u.id=m.user_id
		WHERE m.user_id=$1 AND m.store_id=$2`, member, f.storeID).Scan(&f.actorID, &f.clerkID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f manualRecoveryFixture) request() live.RecoverCommentInput {
	return live.RecoverCommentInput{StoreID: f.storeID, EventID: f.eventID, CommentID: f.commentID, ActorID: f.actorID}
}

func (f manualRecoveryFixture) queue(t *testing.T) {
	t.Helper()
	r, err := f.service.RecoverComment(t.Context(), f.request())
	if err != nil || r.Status != "queued" {
		t.Fatalf("queue: %+v %v", r, err)
	}
}

func (f manualRecoveryFixture) outcome(t *testing.T, result, status string) {
	t.Helper()
	comments, err := f.service.ListCommentsByEvent(t.Context(), f.eventID, f.storeID, "", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range comments {
		if c.ID == f.commentID {
			if c.Result != result || c.RecoveryStatus != status {
				t.Fatalf("comment outcome: %+v", c)
			}
			if status == "rejected" && c.RecoveryReason == "" {
				t.Fatal("missing durable refusal")
			}
			return
		}
	}
	t.Fatal("original comment disappeared")
}

func (f manualRecoveryFixture) assertPurchase(t *testing.T, quantity int) {
	t.Helper()
	var got, stock, events, comments, carts int
	err := testPool.QueryRow(t.Context(), `SELECT p.stock,
		(SELECT COALESCE(sum(quantity),0) FROM cart_items WHERE cart_id=$2 AND product_id=$1),
		(SELECT count(*) FROM cart_item_events WHERE platform_comment_id=$3),
		(SELECT count(*) FROM live_comments WHERE platform_comment_id=$3),
		(SELECT count(*) FROM carts WHERE event_id=$4 AND platform_user_id=$5)
		FROM products p WHERE p.id=$1`, f.productID, f.cartID, f.input.CommentID, f.eventID, f.input.UserID).
		Scan(&stock, &got, &events, &comments, &carts)
	if err != nil {
		t.Fatal(err)
	}
	if got != quantity || stock != 10-quantity || events != quantity || comments != 1 || carts != 1 {
		t.Fatalf("quantity=%d stock=%d events=%d comments=%d carts=%d", got, stock, events, comments, carts)
	}
}

func TestManualCommentRecoveryIncidentAndConcurrentRedelivery(t *testing.T) {
	f := seedManualRecovery(t)
	var wg sync.WaitGroup
	statuses := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			r, err := f.service.RecoverComment(t.Context(), f.request())
			if err != nil {
				t.Error(err)
				return
			}
			statuses <- r.Status
		})
	}
	wg.Wait()
	close(statuses)
	queued := 0
	for status := range statuses {
		if status == "queued" {
			queued++
		} else if status != "already_queued" {
			t.Errorf("status %s", status)
		}
	}
	if queued != 1 {
		t.Fatalf("queued=%d", queued)
	}
	f.outcome(t, "recovery_pending", "pending")
	// Webhook input and a changed media binding cannot replace the authorization.
	duplicate := f.input
	duplicate.Text = "1923 X 9"
	duplicate.UserID = "other-buyer"
	duplicate.MediaID = "reused-media"
	for range 12 {
		wg.Go(func() {
			if err := f.service.ProcessInstagramCommentDelivery(t.Context(), duplicate); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.assertPurchase(t, 1)
	f.outcome(t, "added_to_cart", "completed")
	r, err := f.service.RecoverComment(t.Context(), f.request())
	if err != nil || r.Status != "already_processed" {
		t.Fatalf("repeat: %+v %v", r, err)
	}
	var original, actor, cart string
	var accepted bool
	if err := testPool.QueryRow(t.Context(), `SELECT manual_recovery->>'originalResult',manual_recovery->>'actorId',
		manual_recovery->>'cartId',accepted_at IS NOT NULL FROM live_comment_work WHERE platform_comment_id=$1`, f.input.CommentID).
		Scan(&original, &actor, &cart, &accepted); err != nil {
		t.Fatal(err)
	}
	if original != "no_product" || actor != f.actorID || cart != f.cartID || accepted {
		t.Fatal("authorization changed original accepted-purchase state")
	}
}

func TestManualCommentRecoveryRefusesUnsafeAdmissionAndWorkerChanges(t *testing.T) {
	cases := []struct {
		name, sql string
		arg       func(manualRecoveryFixture) string
		code      int
	}{
		{"paid", `UPDATE carts SET payment_status='paid' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"expired", `UPDATE carts SET status='expired' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"deadline", `UPDATE carts SET expires_at=now()-interval '1 minute' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"cancelled", `UPDATE carts SET status='cancelled' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"cancelled then reopened", `UPDATE carts SET status='active',cancellation_reverted_at=now() WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"cart created after session", `UPDATE carts SET created_at=now()-interval '5 minutes' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"cart created before session", `UPDATE carts SET created_at=now()-interval '2 hours' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"fiscal", `UPDATE carts SET erp_order_status='faturado' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"review", `UPDATE carts SET payment_review_required=true WHERE id=$1`, func(f manualRecoveryFixture) string { return f.cartID }, 409},
		{"event ended", `UPDATE live_events SET status='ended' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.eventID }, 409},
		{"event elapsed", `UPDATE live_events SET ends_at=now()-interval '1 minute' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.eventID }, 409},
		{"paused", `UPDATE live_sessions SET processing_paused=true WHERE id=$1`, func(f manualRecoveryFixture) string { return f.sessionID }, 409},
		{"received after session", `UPDATE live_sessions SET ended_at=now()-interval '30 minutes' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.sessionID }, 409},
		{"deleted", `UPDATE live_comments SET deleted_at=now() WHERE id=$1`, func(f manualRecoveryFixture) string { return f.commentID }, 404},
		{"inactive product", `UPDATE products SET active=false WHERE id=$1`, func(f manualRecoveryFixture) string { return f.productID }, 422},
		{"changed keyword", `UPDATE products SET keyword='9999' WHERE id=$1`, func(f manualRecoveryFixture) string { return f.productID }, 422},
		{"blocked", `INSERT INTO blocked_handles(store_id,platform_handle) VALUES($1,'buyer')`, func(f manualRecoveryFixture) string { return f.storeID }, 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, worker := range []bool{false, true} {
				t.Run(fmt.Sprint("worker=", worker), func(t *testing.T) {
					f := seedManualRecovery(t)
					if worker {
						f.queue(t)
					}
					bindingExec(t, tc.sql, tc.arg(f))
					if worker {
						err := f.service.ProcessInstagramComment(t.Context(), f.input)
						var rejected *live.CommentRecoveryRejected
						if !errors.As(err, &rejected) {
							t.Fatalf("worker refusal: %v", err)
						}
						if tc.name != "deleted" {
							f.outcome(t, "no_product", "rejected")
						}
					} else {
						_, err := f.service.RecoverComment(t.Context(), f.request())
						var domain *httpx.ServiceError
						if !errors.As(err, &domain) || domain.Code != tc.code {
							t.Fatalf("admission: %v want %d", err, tc.code)
						}
					}
					f.assertPurchase(t, 0)
				})
			}
		})
	}
}

func TestManualCommentRecoveryHTTPPermissionsAndDTO(t *testing.T) {
	for _, tc := range []struct {
		name, role, body string
		status           int
	}{
		{"owner", "owner", `{"confirmed":true}`, 200},
		{"admin", "admin", `{"confirmed":true}`, 200},
		{"member", "member", `{"confirmed":true}`, 403},
		{"false", "owner", `{"confirmed":false}`, 422},
		{"missing", "owner", `{}`, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedManualRecovery(t)
			bindingExec(t, `UPDATE memberships SET role=$2 WHERE id=$1`, f.actorID, tc.role)
			app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
			app.Use(func(c *fiber.Ctx) error { c.Locals("user_id", f.clerkID); return c.Next() })
			group := app.Group("/stores/:storeId", httpx.StoreAccessMiddleware(user.NewRepository(sqlc.New(testPool))))
			live.NewHandler(f.service, nil).RegisterRoutes(group)
			path := fmt.Sprintf("/stores/%s/lives/%s/comments/%s/recover", f.storeID, f.eventID, f.commentID)
			req := httptest.NewRequest("POST", path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
			}
			if tc.status == 200 {
				var got struct {
					Data struct {
						Status string `json:"status"`
					} `json:"data"`
				}
				if err := json.Unmarshal(body, &got); err != nil || got.Data.Status != "queued" {
					t.Fatalf("response %s: %v", body, err)
				}
			}
		})
	}
	// Authenticated owner of another store still cannot recover this comment.
	f := seedManualRecovery(t)
	other := seedManualRecovery(t)
	input := f.request()
	input.ActorID = other.actorID
	_, err := f.service.RecoverComment(t.Context(), input)
	var domain *httpx.ServiceError
	if !errors.As(err, &domain) || domain.Code != 403 {
		t.Fatalf("foreign membership: %v", err)
	}
	input.StoreID = other.storeID
	_, err = f.service.RecoverComment(t.Context(), input)
	if !errors.As(err, &domain) || domain.Code != 404 {
		t.Fatalf("foreign comment: %v", err)
	}
}

type manualRecoveryBilling struct{ blocked bool }

func (g *manualRecoveryBilling) IsStoreBlocked(context.Context, string) bool { return g.blocked }

func TestManualCommentRecoveryBillingAndTransientSync(t *testing.T) {
	f := seedManualRecovery(t)
	gate := &manualRecoveryBilling{blocked: true}
	f.service.SetBillingGate(gate)
	if _, err := f.service.RecoverComment(t.Context(), f.request()); err == nil {
		t.Fatal("blocked subscription accepted")
	}
	gate.blocked = false
	f.queue(t)
	bindingExec(t, `INSERT INTO cart_erp_edits(cart_id,revision,synced_revision) VALUES($1,1,0)`, f.cartID)
	if err := f.service.ProcessInstagramComment(t.Context(), f.input); err == nil {
		t.Fatal("pending edit allowed")
	}
	f.outcome(t, "recovery_pending", "pending")
	f.assertPurchase(t, 0)
	bindingExec(t, `UPDATE cart_erp_edits SET synced_revision=revision WHERE cart_id=$1`, f.cartID)
	makeCommentRetryDue(t, f.input.CommentID)
	gate.blocked = true
	if err := f.service.ProcessInstagramComment(t.Context(), f.input); err == nil {
		t.Fatal("worker bypassed subscription")
	}
	f.outcome(t, "no_product", "rejected")
}

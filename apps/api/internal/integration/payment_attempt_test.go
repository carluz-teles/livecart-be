//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"livecart/apps/api/internal/events"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/httpx"
)

func unpaidPaymentFixture(t *testing.T) finFixture {
	t.Helper()
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(context.Background(), `UPDATE carts SET payment_status='pending',paid_at=NULL WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	return fx
}
func paymentFact(t *testing.T, fx finFixture, id string) events.Envelope {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"cart_id": fx.cartID, "store_id": fx.storeID, "payment_id": id})
	if err != nil {
		t.Fatal(err)
	}
	return events.Envelope{Name: events.CartPaid, Source: "pagarme", DedupKey: "cart.paid:" + id, Payload: raw}
}
func TestPaymentAttempt_ConcurrentDuplicateBooksMoneyOnce(t *testing.T) {
	fx := unpaidPaymentFixture(t)
	ctx := context.Background()
	fact := paymentFact(t, fx, fx.cartID+"-charge")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := testRepo.UpdateCartPaymentStatus(ctx, fx.cartID, "paid", fx.cartID+"-charge", nil, "pix", 1000, fact)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, paymentdomain.ErrStalePayment) {
			t.Fatal(err)
		}
	}
	var amount int64
	var rows, facts int
	if err := testPool.QueryRow(ctx, `SELECT paid_amount_cents,(SELECT COUNT(*) FROM cart_payments WHERE cart_id=c.id),(SELECT COUNT(*) FROM event_outbox WHERE dedup_key=$2) FROM carts c WHERE c.id=$1`, fx.cartID, fact.DedupKey).Scan(&amount, &rows, &facts); err != nil {
		t.Fatal(err)
	}
	if amount != 1000 || rows != 1 || facts != 1 {
		t.Fatalf("amount=%d ledger=%d facts=%d", amount, rows, facts)
	}
}
func TestPaymentAttempt_FactFailureRollsBackMoney(t *testing.T) {
	fx := unpaidPaymentFixture(t)
	ctx := context.Background()
	fact := paymentFact(t, fx, "rollback-"+fx.cartID)
	fact.EventID = "invalid-uuid"
	if _, err := testRepo.UpdateCartPaymentStatus(ctx, fx.cartID, "paid", fact.DedupKey, nil, "pix", 1000, fact); err == nil {
		t.Fatal("expected outbox failure")
	}
	var amount int64
	var paid, rows int
	if err := testPool.QueryRow(ctx, `SELECT paid_amount_cents,(SELECT paid_quantity FROM cart_items WHERE cart_id=c.id),(SELECT COUNT(*) FROM cart_payments WHERE cart_id=c.id) FROM carts c WHERE c.id=$1`, fx.cartID).Scan(&amount, &paid, &rows); err != nil {
		t.Fatal(err)
	}
	if amount != 0 || paid != 0 || rows != 0 {
		t.Fatalf("partial commit: amount=%d paid=%d rows=%d", amount, paid, rows)
	}
}
func TestPaymentAttempt_ObsoleteQuoteRecordsMoneyWithoutReleasingNewItems(t *testing.T) {
	fx := unpaidPaymentFixture(t)
	ctx := context.Background()
	paymentID := "old-" + fx.cartID
	var integrationID string
	if err := testPool.QueryRow(ctx, `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'payment','pagarme','active') RETURNING id::text`, fx.storeID).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO payment_attempts(cart_id,integration_id,provider,amount_cents,cart_fingerprint,payment_id) VALUES($1,$2,'pagarme',1000,cart_payment_fingerprint($1),$3)`, fx.cartID, integrationID, paymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cart_items SET quantity=2 WHERE cart_id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	fact := paymentFact(t, fx, paymentID)
	_, err := testRepo.UpdateCartPaymentStatus(ctx, fx.cartID, "paid", paymentID, nil, "pix", 1000, fact)
	if !errors.Is(err, paymentdomain.ErrStalePayment) {
		t.Fatalf("want review, got %v", err)
	}
	var review bool
	var amount int64
	var paid, facts int
	if err := testPool.QueryRow(ctx, `SELECT payment_review_required,paid_amount_cents,(SELECT paid_quantity FROM cart_items WHERE cart_id=c.id),(SELECT COUNT(*) FROM event_outbox WHERE dedup_key=$2) FROM carts c WHERE c.id=$1`, fx.cartID, fact.DedupKey).Scan(&review, &amount, &paid, &facts); err != nil {
		t.Fatal(err)
	}
	if !review || amount != 1000 || paid != 0 || facts != 0 {
		t.Fatalf("review=%v amount=%d paid=%d facts=%d", review, amount, paid, facts)
	}
	manual := paymentFact(t, fx, "manual:"+fx.cartID)
	manual.Source = events.SourceInternal
	if _, err := testRepo.UpdateCartPaymentStatus(ctx, fx.cartID, "paid", "manual:"+fx.cartID, nil, "manual", 2000, manual); err == nil {
		t.Fatal("manual confirmation must not count reviewed money again")
	} else {
		var apiErr *httpx.ServiceError
		if !errors.As(err, &apiErr) || apiErr.Reason != string(httpx.CodePaymentReviewRequired) {
			t.Fatalf("manual review guard: %v", err)
		}
	}
	// Cancelling an older unpaid charge cannot refund this balance either.
	_, err = testRepo.UpdateCartPaymentStatus(ctx, fx.cartID, "refunded", "unrelated", nil, "pix", 1000)
	if !errors.Is(err, paymentdomain.ErrStalePayment) {
		t.Fatalf("stale cancellation: %v", err)
	}
}

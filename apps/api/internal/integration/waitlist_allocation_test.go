package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hibiken/asynq"
	"livecart/apps/api/internal/events"
	"sync"
	"testing"
	"time"

	"livecart/apps/api/internal/inventory"
)

func TestWaitlistAllocationGlobalFIFO(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 2, 0)
	oldest := seedQueueWaiter(t, fx, productID, 1)
	var otherEvent string
	if err := testPool.QueryRow(ctx, `INSERT INTO live_events(store_id,status,title,ends_at) VALUES($1,'active','Other event',now()+interval '1 day') RETURNING id::text`, fx.storeID).Scan(&otherEvent); err != nil {
		t.Fatal(err)
	}
	other := scaleFixture{storeID: fx.storeID, eventID: otherEvent}
	newest := seedQueueWaiter(t, other, productID, 1)
	if _, err := testPool.Exec(ctx, `UPDATE carts SET never_expires=true WHERE id=$1`, newest); err != nil {
		t.Fatal(err)
	}
	visible, err := testRepo.ListActiveByCart(ctx, newest)
	if err != nil || len(visible) != 1 || visible[0].Position != 2 {
		t.Fatalf("cross-event visible position: %+v %v", visible, err)
	}
	first, err := testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	if err != nil || first == nil || first.CartID != oldest {
		t.Fatalf("oldest first: %+v %v", first, err)
	}
	visible, err = testRepo.ListActiveByCart(ctx, newest)
	if err != nil || len(visible) != 1 || visible[0].Position != 1 {
		t.Fatalf("position after older allocation: %+v %v", visible, err)
	}
	second, err := testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	if err != nil || second == nil || second.CartID != newest {
		t.Fatalf("VIP kept own FIFO position: %+v %v", second, err)
	}
	if productStock(t, productID) != 0 || unitsHeldAvailable(t, productID) != 2 {
		t.Fatal("allocation conservation failed")
	}
}

func TestWaitlistAllocationPartialKeepsOriginalPriceAndDeadline(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 1, 1)
	var cartID, requestID string
	if err := testPool.QueryRow(ctx, `SELECT cart_id::text,id::text FROM waitlist_items WHERE product_id=$1`, productID).Scan(&cartID, &requestID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Microsecond)
	for _, q := range []string{
		`UPDATE cart_items SET quantity=3,waitlisted_quantity=3 WHERE product_id=$1`,
		`UPDATE waitlist_items SET quantity=3,original_quantity=3 WHERE product_id=$1`,
		`UPDATE products SET price=1800 WHERE id=$1`,
	} {
		if _, err := testPool.Exec(ctx, q, productID); err != nil {
			t.Fatal(err)
		}
	}
	setCartExpiresAt(t, cartID, deadline)
	next := seedQueueWaiter(t, fx, productID, 2)
	first, err := testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	if err != nil || first == nil || first.UnitPrice != 1000 || first.Quantity != 1 || first.Remaining != 2 {
		t.Fatalf("partial %+v %v", first, err)
	}
	var gotDeadline time.Time
	if err = testPool.QueryRow(ctx, `SELECT expires_at FROM carts WHERE id=$1`, cartID).Scan(&gotDeadline); err != nil {
		t.Fatal(err)
	}
	if !gotDeadline.Equal(deadline) {
		t.Fatalf("deadline changed %v -> %v", deadline, gotDeadline)
	}
	if _, err = testPool.Exec(ctx, `UPDATE products SET stock=3 WHERE id=$1`, productID); err != nil {
		t.Fatal(err)
	}
	second, err := testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	if err != nil || second == nil || second.WaitlistItemID != requestID || second.Quantity != 2 || second.Remaining != 0 {
		t.Fatalf("remaining kept priority %+v %v", second, err)
	}
	third, err := testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	if err != nil || third == nil || third.CartID != next {
		t.Fatalf("next buyer %+v %v", third, err)
	}
	var fulfilled, original, facts int
	if err = testPool.QueryRow(ctx, `SELECT fulfilled_quantity,original_quantity FROM waitlist_items WHERE id=$1`, requestID).Scan(&fulfilled, &original); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE name='waitlist.notified' AND payload->>'waitlist_item_id'=$1`, requestID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if fulfilled != 3 || original != 3 || facts != 2 {
		t.Fatalf("history fulfilled=%d original=%d facts=%d", fulfilled, original, facts)
	}
}

func TestWaitlistAllocationBusyHeadIsNotOvertaken(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 2, 2)
	var head string
	if err := testPool.QueryRow(ctx, `SELECT cart_id::text FROM waitlist_items WHERE product_id=$1 ORDER BY created_at,queue_sequence LIMIT 1`, productID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	release, ok, err := testRepo.AcquireCartFinalisationLock(ctx, head)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	result, err := testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	release()
	if result != nil || !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) {
		t.Fatalf("busy head %+v %v", result, err)
	}
	if productStock(t, productID) != 2 || unitsHeldAvailable(t, productID) != 0 {
		t.Fatal("busy oldest buyer was overtaken")
	}
	result, err = testRepo.PromoteNextWaitlistEntry(ctx, fx.storeID, productID)
	if err != nil || result == nil || result.CartID != head {
		t.Fatalf("retry %+v %v", result, err)
	}
}

func TestWaitlistAllocationConcurrentStockConservation(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 7, 15)
	errs := make(chan error, 30)
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := testRepo.PromoteNextWaitlistEntry(context.Background(), fx.storeID, productID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := productStock(t, productID); got != 0 {
		t.Fatalf("stock=%d", got)
	}
	if got := unitsHeldAvailable(t, productID); got != 7 {
		t.Fatalf("allocated=%d want7", got)
	}
	var maxPromoted, minWaiting int64
	if err := testPool.QueryRow(t.Context(), `SELECT max(queue_sequence) FILTER(WHERE status='notified'),min(queue_sequence) FILTER(WHERE status='waiting') FROM waitlist_items WHERE product_id=$1`, productID).Scan(&maxPromoted, &minWaiting); err != nil {
		t.Fatal(err)
	}
	if maxPromoted >= minWaiting {
		t.Fatal("concurrent promotion violated FIFO")
	}
}

func TestWaitlistAllocationSkipsTerminalCartsAndDoesNotClampNegativeStock(t *testing.T) {
	requireDB(t)
	for _, status := range []string{"paid", "expired", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			fx := seedScaleEvent(t)
			productID := seedSoldOutProductWithQueue(t, fx, 1, 1)
			var oldest string
			if err := testPool.QueryRow(t.Context(), `SELECT cart_id::text FROM waitlist_items WHERE product_id=$1`, productID).Scan(&oldest); err != nil {
				t.Fatal(err)
			}
			q := `UPDATE carts SET status=$2 WHERE id=$1`
			if status == "paid" {
				q = `UPDATE carts SET payment_status=$2 WHERE id=$1`
			}
			if _, err := testPool.Exec(t.Context(), q, oldest, status); err != nil {
				t.Fatal(err)
			}
			next := seedQueueWaiter(t, fx, productID, 2)
			result, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, productID)
			if err != nil || result == nil || result.CartID != next {
				t.Fatalf("terminalhead %+v %v", result, err)
			}
		})
	}
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, -3, 1)
	result, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, productID)
	if err != nil || result != nil || productStock(t, productID) != -3 {
		t.Fatalf("negative stock %+v %v", result, err)
	}
}

func TestWaitlistCancellationAtomicAndIdempotent(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 1)
	var cartID, requestID string
	if err := testPool.QueryRow(t.Context(), `SELECT cart_id::text,id::text FROM waitlist_items WHERE product_id=$1`, productID).Scan(&cartID, &requestID); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		changed, err := testRepo.CancelWaitingRequest(t.Context(), requestID, cartID)
		if err != nil || changed != (i == 0) {
			t.Fatalf("attempt%d changed=%v error=%v", i, changed, err)
		}
	}
	var cartLines, waiting, cancelled int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM cart_items WHERE cart_id=$1`, cartID).Scan(&cartLines); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT quantity,cancelled_quantity FROM waitlist_items WHERE id=$1`, requestID).Scan(&waiting, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cartLines != 0 || waiting != 0 || cancelled != 1 || productStock(t, productID) != 0 {
		t.Fatal(fmt.Sprintf("lines=%d waiting=%d cancelled=%d", cartLines, waiting, cancelled))
	}
}

func TestWaitlistAllocationOutboxFailureRollsBackAllState(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 1, 1)
	// Deliberately reject this allocation fact, after the cart and stock writes.
	// Other facts and fixtures remain unaffected by this local test constraint.
	q := fmt.Sprintf(`ALTER TABLE event_outbox ADD CONSTRAINT test_waitlist_outbox_failure CHECK (name<>'waitlist.notified' OR payload->>'product_id'<>'%s')`, productID)
	if _, err := testPool.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS test_waitlist_outbox_failure`); err != nil {
			t.Error(err)
		}
	})
	result, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, productID)
	if err == nil || result != nil {
		t.Fatalf("expected transaction failure: %+v %v", result, err)
	}
	if productStock(t, productID) != 1 || unitsHeldAvailable(t, productID) != 0 || countByStatus(t, productID, "waiting") != 1 {
		t.Fatal("failed outbox insert committed partial allocation")
	}
}

func TestWaitlistCancellationPreservesAlreadyPromotedUnits(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 1, 1)
	var requestID, cartID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text,cart_id::text FROM waitlist_items WHERE product_id=$1`, productID).Scan(&requestID, &cartID); err != nil {
		t.Fatal(err)
	}
	// This is one original three-unit lot, as written by comment ingestion.
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET quantity=3,waitlisted_quantity=3 WHERE cart_id=$1`, cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE waitlist_items SET quantity=3,original_quantity=3 WHERE id=$1`, requestID); err != nil {
		t.Fatal(err)
	}
	result, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, productID)
	if err != nil || result == nil || result.Quantity != 1 {
		t.Fatalf("partial %+v %v", result, err)
	}
	if changed, err := testRepo.CancelWaitingRequest(t.Context(), requestID, cartID); err != nil || !changed {
		t.Fatalf("cancel %v %v", changed, err)
	}
	var qty, waiting, fulfilled, cancelled int
	if err := testPool.QueryRow(t.Context(), `SELECT quantity,waitlisted_quantity FROM cart_items WHERE cart_id=$1`, cartID).Scan(&qty, &waiting); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT fulfilled_quantity,cancelled_quantity FROM waitlist_items WHERE id=$1`, requestID).Scan(&fulfilled, &cancelled); err != nil {
		t.Fatal(err)
	}
	if qty != 1 || waiting != 0 || fulfilled != 1 || cancelled != 2 || productStock(t, productID) != 0 {
		t.Fatalf("qty=%d waiting=%d fulfilled=%d cancelled=%d", qty, waiting, fulfilled, cancelled)
	}
}

func TestWaitlistAllocationJoinedHostControlsPayment(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 2, 1)
	var child string
	if err := testPool.QueryRow(t.Context(), `SELECT cart_id::text FROM waitlist_items WHERE product_id=$1`, productID).Scan(&child); err != nil {
		t.Fatal(err)
	}
	host := seedHolderCart(t, fx, productID, 1)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET joined_to_cart_id=$2 WHERE id=$1`, child, host); err != nil {
		t.Fatal(err)
	}
	release, acquired, err := testRepo.AcquireCartFinalisationLock(t.Context(), host)
	if err != nil || !acquired {
		t.Fatalf("host lock %v %v", acquired, err)
	}
	result, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, productID)
	release()
	if result != nil || !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) {
		t.Fatalf("host payment lock bypassed %+v %v", result, err)
	}
	if _, err = testPool.Exec(t.Context(), `UPDATE carts SET payment_status='paid' WHERE id=$1`, host); err != nil {
		t.Fatal(err)
	}
	result, err = testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, productID)
	if err != nil || result != nil || productStock(t, productID) != 2 {
		t.Fatalf("paid host received new item %+v %v", result, err)
	}
}

func TestWaitlistReopenPreservesPriorityAndPriceLots(t *testing.T) {
	requireDB(t)
	cancelled := semearCarrinhoCancelado(t, 1, 1)
	fx := scaleFixture{storeID: cancelled.storeID, eventID: cancelled.eventID}
	oldest := seedQueueWaiter(t, fx, cancelled.produto, 1)
	// Different price on the second addition, preserved while the cart was closed.
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT cart_item_request_price($1,$2,2700,NULL)`, cancelled.cartID, cancelled.produto); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `UPDATE cart_items SET quantity=quantity+1 WHERE cart_id=$1 AND product_id=$2`, cancelled.cartID, cancelled.produto); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	result, err := testRepo.ReopenCancelledCartFromERP(t.Context(), cancelled.cartID, cancelled.storeID)
	if err != nil || result.Recuperadas != 0 || result.EmFila != 2 {
		t.Fatalf("reopen %+v %v", result, err)
	}
	next, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, cancelled.produto)
	if err != nil || next == nil || next.CartID != oldest {
		t.Fatalf("older request lost priority %+v %v", next, err)
	}
	var requests, priceTotal int
	if err = testPool.QueryRow(t.Context(), `SELECT count(*),sum(quantity*unit_price) FROM waitlist_items WHERE cart_id=$1 AND status='waiting'`, cancelled.cartID).Scan(&requests, &priceTotal); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || priceTotal != 4700 {
		t.Fatalf("requests=%d total=%d", requests, priceTotal)
	}
}

func TestWaitlistStockReleaseRetriesAndRepeatedReleasesWakeQueue(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 1, 2)
	var head string
	if err := testPool.QueryRow(t.Context(), `SELECT cart_id::text FROM waitlist_items WHERE product_id=$1 ORDER BY created_at,queue_sequence LIMIT 1`, productID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"product_id": productID})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(events.Envelope{Name: events.StockReleased, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	task := asynq.NewTask(string(events.StockReleased), raw)
	svc := scaleService()
	release, acquired, err := testRepo.AcquireCartFinalisationLock(t.Context(), head)
	if err != nil || !acquired {
		t.Fatalf("headlock %v %v", acquired, err)
	}
	err = svc.ReactStockReleased(t.Context(), task)
	release()
	if !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) {
		t.Fatalf("durable release swallowed contention: %v", err)
	}
	if err = svc.ReactStockReleased(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err = svc.ReactStockReleased(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if productStock(t, productID) != 0 || unitsHeldAvailable(t, productID) != 1 {
		t.Fatal("release redelivery duplicated allocation")
	}
	event := StockEventParams{ProductID: productID, CartID: head, EventID: fx.eventID, Quantity: 1, Op: "cart_remove"}
	if err = testRepo.EmitStockReleased(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err = testRepo.EmitStockReleased(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	var facts int
	if err = testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox WHERE name='stock.released' AND payload->>'product_id'=$1`, productID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 2 {
		t.Fatalf("distinct releases lost a wakeup: %d facts", facts)
	}
	if err = testRepo.IncrementProductStock(t.Context(), productID, 1); err != nil {
		t.Fatal(err)
	}
	if err = svc.ReactStockReleased(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if productStock(t, productID) != 0 || unitsHeldAvailable(t, productID) != 2 {
		t.Fatal("second stock release did not advance queue")
	}
}

//go:build integration

package live

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"sync"
	"testing"
	"time"
)

func commentItemFixture(t *testing.T, stock int) (*Service, AddToCartInput) {
	t.Helper()
	requireDB(t)
	eventID := seedEvent(t)
	input := AddToCartInput{EventID: eventID, SessionID: seedSession(t, eventID, 1),
		ProductID: seedProduct(t, eventID, 390), ProductPrice: 390, Quantity: 2, PlatformUserID: "buyer", PlatformHandle: "buyer"}
	if err := testPool.QueryRow(context.Background(), `SELECT store_id::text FROM live_events WHERE id=$1`, eventID).Scan(&input.StoreID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE products SET stock=$2 WHERE id=$1`, input.ProductID, stock); err != nil {
		t.Fatal(err)
	}
	return &Service{repo: testRepo, logger: zap.NewNop()}, input
}

func TestApplyCommentItem_ConcurrentRedelivery(t *testing.T) {
	svc, input := commentItemFixture(t, 10)
	id := fmt.Sprintf("same-%d", time.Now().UnixNano())
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := svc.ApplyCommentItem(context.Background(), input, id); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var stock, quantity, entries int
	if err := testPool.QueryRow(context.Background(), `SELECT stock FROM products WHERE id=$1`, input.ProductID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT COALESCE(SUM(quantity),0),COUNT(*) FROM cart_item_events WHERE platform_comment_id=$1`, id).Scan(&quantity, &entries); err != nil {
		t.Fatal(err)
	}
	if stock != 8 || quantity != 2 || entries != 1 {
		t.Fatalf("stock=%d quantity=%d entries=%d; want 8/2/1", stock, quantity, entries)
	}
}

func TestApplyCommentItem_StockAndWaitlistCommitTogether(t *testing.T) {
	svc, input := commentItemFixture(t, 1)
	id := fmt.Sprintf("partial-%d", time.Now().UnixNano())
	result, err := svc.ApplyCommentItem(context.Background(), input, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Quantity != 2 || result.WaitlistedQuantity != 1 {
		t.Fatalf("allocation: %+v", result)
	}
	again, err := svc.ApplyCommentItem(context.Background(), input, id)
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyApplied || again.Quantity != 2 || again.WaitlistedQuantity != 1 {
		t.Fatalf("retry: %+v", again)
	}
	var pending bool
	var waiting, stock int
	if err = testPool.QueryRow(context.Background(), `SELECT erp_pending_since IS NOT NULL FROM cart_items WHERE cart_id=$1 AND product_id=$2`, result.CartID, input.ProductID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(context.Background(), `SELECT COALESCE(SUM(quantity),0) FROM waitlist_items WHERE cart_id=$1`, result.CartID).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(context.Background(), `SELECT stock FROM products WHERE id=$1`, input.ProductID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if !pending || waiting != 1 || stock != 0 {
		t.Fatalf("pending=%v waiting=%d stock=%d", pending, waiting, stock)
	}
}

func TestApplyCommentItem_FailedWriteDoesNotConsumeStock(t *testing.T) {
	svc, input := commentItemFixture(t, 10)
	cart, _, err := svc.getOrCreateCartForItem(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.SessionID = "00000000-0000-0000-0000-000000000001" // FK failure after the stock update.
	_, err = svc.repo.applyCommentItem(context.Background(), input, "rollback-"+input.ProductID, cart, false)
	if err == nil {
		t.Fatal("expected foreign-key failure")
	}
	var stock, items int
	if err = testPool.QueryRow(context.Background(), `SELECT stock FROM products WHERE id=$1`, input.ProductID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM cart_items WHERE cart_id=$1`, cart.ID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if stock != 10 || items != 0 {
		t.Fatalf("stock=%d items=%d after rollback", stock, items)
	}
}

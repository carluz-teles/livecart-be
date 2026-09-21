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

func TestApplyCommentItemReplayReflectsERPReconciliation(t *testing.T) {
	svc, input := commentItemFixture(t, 10)
	id := "reconcile-" + input.ProductID
	first, err := svc.ApplyCommentItem(t.Context(), input, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status='faturado'
		WHERE id=$1`, first.CartID); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.ApplyCommentItem(t.Context(), input, id)
	if err != nil || !replay.ERPBlocked || replay.ERPConfirmed || !replay.AlreadyApplied {
		t.Fatalf("pending closed order: %+v %v", replay, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET erp_pending_since=NULL,
		erp_confirmed_quantity=quantity WHERE cart_id=$1`, first.CartID); err != nil {
		t.Fatal(err)
	}
	replay, err = svc.ApplyCommentItem(t.Context(), input, id)
	if err != nil || !replay.ERPConfirmed || replay.CartID != first.CartID {
		t.Fatalf("confirmed replay: %+v %v", replay, err)
	}
	var stock int
	if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, input.ProductID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 8 || itemCount(t, first.CartID) != 1 {
		t.Fatal("replay changed stock or cart items")
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

func TestApplyCommentItem_NewWaitingUnitsKeepNewPriorityAndPrice(t *testing.T) {
	svc, input := commentItemFixture(t, 0)
	input.Quantity = 1
	first, err := svc.ApplyCommentItem(t.Context(), input, "first-"+input.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	input.PlatformUserID = "another-buyer"
	input.PlatformHandle = "another-buyer"
	if _, err = svc.ApplyCommentItem(t.Context(), input, "middle-"+input.ProductID); err != nil {
		t.Fatal(err)
	}
	input.PlatformUserID = "buyer"
	input.PlatformHandle = "buyer"
	input.Quantity = 2
	input.ProductPrice = 500
	last, err := svc.ApplyCommentItem(t.Context(), input, "last-"+input.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	if last.CartID != first.CartID || last.WaitlistedQuantity != 2 {
		t.Fatalf("new request %+v", last)
	}
	// A delivered comment can repeat without creating another request or lot.
	if replay, err := svc.ApplyCommentItem(t.Context(), input, "last-"+input.ProductID); err != nil || !replay.AlreadyApplied {
		t.Fatalf("replay %+v %v", replay, err)
	}
	rows, err := testPool.Query(t.Context(), `SELECT platform_user_id,quantity,unit_price FROM waitlist_items WHERE product_id=$1 ORDER BY created_at,queue_sequence`, input.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := []struct {
		user     string
		quantity int
		price    int64
	}{{"buyer", 1, 390}, {"another-buyer", 1, 390}, {"buyer", 2, 500}}
	for i := 0; rows.Next(); i++ {
		if i >= len(expected) {
			t.Fatal("duplicate waiting request")
		}
		var user string
		var quantity int
		var price int64
		if err = rows.Scan(&user, &quantity, &price); err != nil {
			t.Fatal(err)
		}
		if user != expected[i].user || quantity != expected[i].quantity || price != expected[i].price {
			t.Fatalf("entry%d = %s/%d/%d", i, user, quantity, price)
		}
	}
	var lots, total int
	if err = testPool.QueryRow(t.Context(), `SELECT count(*),sum(l.quantity*l.unit_price) FROM cart_item_price_lots l JOIN cart_items ci ON ci.id=l.cart_item_id WHERE ci.cart_id=$1`, first.CartID).Scan(&lots, &total); err != nil {
		t.Fatal(err)
	}
	if lots != 2 || total != 1390 {
		t.Fatalf("price lots=%d total=%d", lots, total)
	}
}

func TestApplyCommentItem_DoesNotOvertakeExistingQueue(t *testing.T) {
	svc, input := commentItemFixture(t, 0)
	if _, err := svc.ApplyCommentItem(t.Context(), input, "old-"+input.ProductID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=2 WHERE id=$1`, input.ProductID); err != nil {
		t.Fatal(err)
	}
	input.PlatformUserID = "new-buyer"
	input.PlatformHandle = "new-buyer"
	result, err := svc.ApplyCommentItem(t.Context(), input, "new-"+input.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	if result.WaitlistedQuantity != 2 {
		t.Fatalf("new buyer bypassed queue: %+v", result)
	}
	var stock, facts int
	if err = testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, input.ProductID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox WHERE name='waitlist.queued' AND payload->>'product_id'=$1`, input.ProductID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if stock != 2 || facts != 2 {
		t.Fatalf("stock=%d durable queue wakeups=%d", stock, facts)
	}
}

func TestApplyCommentItem_JoinedHostPaymentWinsBeforeAdmission(t *testing.T) {
	svc, input := commentItemFixture(t, 2)
	child, _, err := svc.getOrCreateCartForItem(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	hostInput := input
	hostInput.PlatformUserID = "host-buyer"
	hostInput.PlatformHandle = "host-buyer"
	host, _, err := svc.getOrCreateCartForItem(t.Context(), hostInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testPool.Exec(t.Context(), `UPDATE carts SET joined_to_cart_id=$2 WHERE id=$1`, child.ID, host.ID); err != nil {
		t.Fatal(err)
	}
	// Payment owns the host row. The comment was already routed to the child,
	// but must wait for the host and re-evaluate its state after payment commits.
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `UPDATE carts SET payment_status='paid' WHERE id=$1`, host.ID); err != nil {
		t.Fatal(err)
	}
	complete := make(chan error, 1)
	go func() {
		_, err := svc.repo.applyCommentItem(t.Context(), input, "host-paid-"+input.ProductID, child, false)
		complete <- err
	}()
	// The result must only be evaluated after committing the authoritative host.
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-complete; err == nil {
		t.Fatal("comment mutated a child of a paid checkout")
	}
	var stock, lines, requests int
	if err = testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, input.ProductID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(t.Context(), `SELECT count(*) FROM cart_items WHERE cart_id=$1`, child.ID).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if err = testPool.QueryRow(t.Context(), `SELECT count(*) FROM waitlist_items WHERE cart_id=$1`, child.ID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if stock != 2 || lines != 0 || requests != 0 {
		t.Fatalf("stock=%d cartlines=%d waitingrequests=%d", stock, lines, requests)
	}
}

func TestApplyCommentItem_JoinedCheckoutKeepsOpenCartThenStartsNewPurchase(t *testing.T) {
	svc, input := commentItemFixture(t, 10)
	input.Quantity = 1
	first, err := svc.ApplyCommentItem(t.Context(), input, "joined-first-"+input.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	hostInput := input
	hostInput.PlatformUserID = "host-owner"
	hostInput.PlatformHandle = "host-owner"
	host, _, err := svc.getOrCreateCartForItem(t.Context(), hostInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testPool.Exec(t.Context(), `UPDATE carts SET joined_to_cart_id=$2 WHERE id=$1`, first.CartID, host.ID); err != nil {
		t.Fatal(err)
	}
	second, err := svc.ApplyCommentItem(t.Context(), input, "joined-second-"+input.ProductID)
	if err != nil || second.CartID != first.CartID {
		t.Fatalf("open host should keep child: %+v %v", second, err)
	}
	if _, err = testPool.Exec(t.Context(), `UPDATE carts SET payment_status='paid' WHERE id=$1`, host.ID); err != nil {
		t.Fatal(err)
	}
	third, err := svc.ApplyCommentItem(t.Context(), input, "joined-third-"+input.ProductID)
	if err != nil || third.CartID == first.CartID || !third.IsNewCart {
		t.Fatalf("paid host needs new purchase: %+v %v", third, err)
	}
	var link, payment string
	var originalQty int
	if err = testPool.QueryRow(t.Context(), `SELECT c.joined_to_cart_id::text,COALESCE(c.payment_status,''),ci.quantity
        FROM carts c JOIN cart_items ci ON ci.cart_id=c.id WHERE c.id=$1`, first.CartID).Scan(&link, &payment, &originalQty); err != nil {
		t.Fatal(err)
	}
	if link != host.ID || payment == "paid" || originalQty != 2 {
		t.Fatalf("original purchase changed: host=%s payment=%s qty=%d", link, payment, originalQty)
	}
}

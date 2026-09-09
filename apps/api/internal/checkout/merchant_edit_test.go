//go:build integration

package checkout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

type editFixture struct {
	cart, token, product, item, store string
	service                           *Service
}

func seedMerchantEdit(t *testing.T) editFixture {
	t.Helper()
	requireDB(t)
	ctx := t.Context()
	f := editFixture{cart: seedCartForPix(t)}
	if err := testPool.QueryRow(ctx, `SELECT e.store_id::text,c.token FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1`, f.cart).Scan(&f.store, &f.token); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'erp','tiny','active')`, f.store); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE carts SET store_id=$2, external_order_id='test-tiny',erp_order_state='open' WHERE id=$1`, f.cart, f.store); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO products(store_id,name,keyword,price,stock,external_source,external_id)
        VALUES($1,'Edit test','1234',1000,8,'tiny','1234') RETURNING id::text`, f.store).Scan(&f.product); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price,erp_confirmed_quantity)
        VALUES($1,$2,2,1000,2) RETURNING id::text`, f.cart, f.product).Scan(&f.item); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM carts WHERE id=$1`, f.cart) })
	f.service = NewService(testRepo, testPool, nil, nil, zap.NewNop())
	return f
}

func TestMerchantEdit_RemovalIsDurableAndKeepsStockUntilAcknowledged(t *testing.T) {
	f := seedMerchantEdit(t)
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	// No integration provider is wired: the merchant HTTP path must return
	// from durable acceptance without doing checkout sweeps or ERP I/O.
	if err := f.service.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
		t.Fatalf("duplicate removal: %v", err)
	}
	var stock, requests, items int
	if err := testPool.QueryRow(ctx, `SELECT stock,(SELECT count(*) FROM cart_erp_edit_requests WHERE cart_id=$2),
        (SELECT count(*) FROM cart_items WHERE cart_id=$2) FROM products WHERE id=$1`, f.product, f.cart).Scan(&stock, &requests, &items); err != nil {
		t.Fatal(err)
	}
	if stock != 8 || requests != 1 || items != 0 {
		t.Fatalf("stock=%d requests=%d items=%d", stock, requests, items)
	}
	if err := cartedit.AssertReady(ctx, testPool, f.cart); err == nil {
		t.Fatal("pending removal permitted payment")
	}
	// A fresh process can load the durable work after the HTTP handler exits.
	owner := uuid.NewString()
	if _, err := testPool.Exec(ctx, `UPDATE cart_erp_edits SET lease_owner=$2,lease_until=now()+interval '3 minutes' WHERE cart_id=$1`, f.cart, owner); err != nil {
		t.Fatal(err)
	}
	fresh := NewService(testRepo, testPool, nil, nil, zap.NewNop())
	if _, err := fresh.finishMerchantEdit(ctx, merchantEditWork{cartID: f.cart, owner: owner, revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 10 {
		t.Fatalf("released stock=%d", stock)
	}
	if _, err := fresh.finishMerchantEdit(ctx, merchantEditWork{cartID: f.cart, owner: owner, revision: 1}); err == nil {
		t.Fatal("ack applied twice")
	}
	if err := cartedit.AssertReady(ctx, testPool, f.cart); err != nil {
		t.Fatal(err)
	}
	// A retry stays idempotent even after the ERP link/configuration changes.
	if _, err := testPool.Exec(ctx, `UPDATE carts SET external_order_id=NULL WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	if err := fresh.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
		t.Fatalf("lost idempotency after ERP unlink: %v", err)
	}

}

func TestMerchantEdit_CoalescesQuantitiesAndDoesNotAllowPayment(t *testing.T) {
	f := seedMerchantEdit(t)
	for _, qty := range []int{3, 4, 1} {
		ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
		if _, err := f.service.queueMerchantEdit(ctx, MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: qty, ByMerchant: true}, "set"); err != nil {
			t.Fatal(err)
		}
	}
	var revision, qty, stock, held int
	if err := testPool.QueryRow(t.Context(), `SELECT w.revision,ci.quantity,p.stock,
        (SELECT sum(retained_quantity) FROM cart_erp_edit_requests WHERE cart_id=w.cart_id)
        FROM cart_erp_edits w JOIN cart_items ci ON ci.cart_id=w.cart_id JOIN products p ON p.id=ci.product_id WHERE w.cart_id=$1`, f.cart).Scan(&revision, &qty, &stock, &held); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || qty != 1 || stock != 6 || held != 3 {
		t.Fatalf("revision=%d qty=%d stock=%d held=%d", revision, qty, stock, held)
	}
	_, err := testRepo.createPaymentAttempt(context.Background(), testPool, f.cart, uuid.NewString(), "pagarme", "pix", 1000,
		[]providers.CheckoutItem{{ID: f.product, Quantity: 1, UnitPrice: 1000}})
	assertEditPendingConflict(t, err)
}

func TestMerchantEdit_RejectsChangedIdempotencyKeyAndInsufficientStock(t *testing.T) {
	f := seedMerchantEdit(t)
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	input := MutateCartItemInput{Token: f.token, ProductID: f.product, Quantity: 1, ByMerchant: true}
	if _, err := f.service.queueMerchantEdit(ctx, input, "add"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.queueMerchantEdit(ctx, input, "add"); err != nil {
		t.Fatal(err)
	}
	input.Quantity = 2
	if _, err := f.service.queueMerchantEdit(ctx, input, "add"); err == nil {
		t.Fatal("key reused with different payload")
	}
	input.Quantity = 999
	if _, err := f.service.queueMerchantEdit(cartedit.WithRequestID(t.Context(), uuid.NewString()), input, "add"); err == nil {
		t.Fatal("oversold stock")
	}
	var qty, revision int
	if err := testPool.QueryRow(ctx, `SELECT ci.quantity,w.revision FROM cart_items ci JOIN cart_erp_edits w ON w.cart_id=ci.cart_id WHERE ci.id=$1`, f.item).Scan(&qty, &revision); err != nil {
		t.Fatal(err)
	}
	if qty != 3 || revision != 1 {
		t.Fatalf("qty=%d revision=%d", qty, revision)
	}
}

type scriptedMerchantERP struct {
	mutate   func(context.Context, string, string) error
	promoted atomic.Int32
}

func (f *scriptedMerchantERP) MutateERPOrderItems(ctx context.Context, cart, store string) error {
	return f.mutate(ctx, cart, store)
}
func (f *scriptedMerchantERP) ProcessWaitlistForProduct(context.Context, string, string, string) {
	f.promoted.Add(1)
}

func dueMerchantEdit(t *testing.T, cart string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET next_attempt_at=now()-interval '1 second' WHERE cart_id=$1`, cart); err != nil {
		t.Fatal(err)
	}
}

func TestMerchantEdit_RetryAfterRestartKeepsStockAndSkipsActiveLease(t *testing.T) {
	f := seedMerchantEdit(t)
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	if _, err := f.service.queueMerchantEdit(ctx, MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: 1, ByMerchant: true}, "set"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fake := &scriptedMerchantERP{mutate: func(ctx context.Context, cart, store string) error {
		if cart != f.cart || store != f.store {
			return fmt.Errorf("wrong tenant")
		}
		calls.Add(1)
		if len(erp.EditedProducts(ctx)) != 1 {
			return fmt.Errorf("lost removed-product ownership")
		}
		return fmt.Errorf("Tiny unavailable")
	}}
	f.service.merchantEditERP = fake
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	st, err := cartedit.Read(t.Context(), testPool, f.cart)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Pending || st.Processing || st.LastError == "" || st.Attempts != 1 {
		t.Fatalf("failed status: %+v", st)
	}
	var stock int
	if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 8 || fake.promoted.Load() != 0 {
		t.Fatalf("stock released on ERP failure: %d", stock)
	}
	f.service.RecoverMerchantEdits(t.Context())
	if calls.Load() != 1 {
		t.Fatal("retry ignored scheduled backoff")
	}
	// Simulate a process dying while it owns the claim. Another replica must
	// respect a live lease, then resume once it expires.
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET lease_owner=$2,lease_until=now()+interval '1 minute',next_attempt_at=now() WHERE cart_id=$1`, f.cart, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	fresh := NewService(testRepo, testPool, nil, nil, zap.NewNop())
	fresh.merchantEditERP = fake
	fresh.RecoverMerchantEdits(t.Context())
	if calls.Load() != 1 {
		t.Fatal("stole active lease")
	}
	fake.mutate = func(context.Context, string, string) error { calls.Add(1); return nil }
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET lease_until=now()-interval '1 second' WHERE cart_id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	fresh.RecoverMerchantEdits(t.Context())
	if err := cartedit.AssertReady(t.Context(), testPool, f.cart); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 9 || fake.promoted.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("stock=%d promotions=%d calls=%d", stock, fake.promoted.Load(), calls.Load())
	}
	fresh.RecoverMerchantEdits(t.Context())
	if calls.Load() != 2 {
		t.Fatal("completed edit replayed")
	}
}

func TestMerchantEdit_ConcurrentWorkersSendLatestGridOnce(t *testing.T) {
	f := seedMerchantEdit(t)
	for _, qty := range []int{3, 4, 1} {
		if _, err := f.service.queueMerchantEdit(cartedit.WithRequestID(t.Context(), uuid.NewString()), MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: qty, ByMerchant: true}, "set"); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	fake := &scriptedMerchantERP{mutate: func(ctx context.Context, cart, store string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		var quantity int
		if err := testPool.QueryRow(ctx, `SELECT quantity FROM cart_items WHERE id=$1`, f.item).Scan(&quantity); err != nil {
			return err
		}
		if quantity != 1 {
			return fmt.Errorf("sent intermediate quantity %d", quantity)
		}
		<-release
		return nil
	}}
	f.service.merchantEditERP = fake
	dueMerchantEdit(t, f.cart)
	done := make(chan struct{})
	go func() { defer close(done); f.service.RecoverMerchantEdits(t.Context()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("worker did not claim")
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); f.service.RecoverMerchantEdits(t.Context()) }()
	}
	wg.Wait()
	_, err := f.service.queueMerchantEdit(cartedit.WithRequestID(t.Context(), uuid.NewString()), MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: 2, ByMerchant: true}, "set")
	close(release)
	<-done
	if err == nil {
		t.Fatal("accepted edit while sending")
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent dispatches: %d", calls.Load())
	}
	var stock int
	if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 9 {
		t.Fatalf("coalesced stock=%d", stock)
	}
}

func TestMerchantEdit_BlocksMirrorShippingAndLatePayment(t *testing.T) {
	f := seedMerchantEdit(t)
	if _, err := f.service.queueMerchantEdit(cartedit.WithRequestID(t.Context(), uuid.NewString()), MutateCartItemInput{Token: f.token, ItemID: f.item, ByMerchant: true}, "remove"); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := testPool.QueryRow(t.Context(), `SELECT erp_seq FROM products WHERE id=$1`, f.product).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	id := pgtype.UUID{Bytes: uuid.MustParse(f.product), Valid: true}
	n, err := testQueries.ApplyERPStockMirror(t.Context(), sqlc.ApplyERPStockMirrorParams{ID: id, SeenSeq: seq, ErpStock: 10})
	if err != nil || n != 0 {
		t.Fatalf("mirror during pending removal: %d %v", n, err)
	}
	if err := testRepo.UpdateCartShipping(t.Context(), testPool, f.cart, &CartShippingSelection{ServiceID: "old", CostCents: 123}); err == nil {
		t.Fatal("accepted stale shipping selection")
	}
	repo := integration.NewRepository(testQueries, testPool)
	if won, err := repo.TransitionCartERPOrderState(t.Context(), f.cart, "open", "reflecting"); err != nil || won {
		t.Fatalf("ERP reflection replaced pending merchant edit: %v %v", won, err)
	}
	_, err = repo.UpdateCartPaymentStatus(t.Context(), f.cart, "paid", "manual-test", nil, "manual", 1000)
	assertEditPendingConflict(t, err)
	other := seedMerchantEdit(t)
	_, err = repo.JoinCartIntoHost(t.Context(), f.cart, other.cart)
	assertEditPendingConflict(t, err)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_review_required=true WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	fake := &scriptedMerchantERP{mutate: func(context.Context, string, string) error { t.Error("mutated order with late payment"); return nil }}
	f.service.merchantEditERP = fake
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	st, err := cartedit.Read(t.Context(), testPool, f.cart)
	if err != nil || !st.Pending || st.LastError == "" {
		t.Fatalf("payment review lost: %+v %v", st, err)
	}
}

func assertEditPendingConflict(t *testing.T, err error) {
	t.Helper()
	var e *httpx.ServiceError
	if !errors.As(err, &e) || e.Code != 409 || e.Reason != string(httpx.CodeCartERPSyncPending) {
		t.Fatalf("expected synchronization conflict, got %v", err)
	}
}

func TestMerchantEdit_ConcurrentDuplicateAdditionReservesOnce(t *testing.T) {
	f := seedMerchantEdit(t)
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- f.service.AddCartItemAsMerchant(ctx, f.token, f.product, 1) }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var quantity, stock, requests int
	if err := testPool.QueryRow(t.Context(), `SELECT ci.quantity,p.stock,(SELECT count(*) FROM cart_erp_edit_requests WHERE cart_id=ci.cart_id)
        FROM cart_items ci JOIN products p ON p.id=ci.product_id WHERE ci.id=$1`, f.item).Scan(&quantity, &stock, &requests); err != nil {
		t.Fatal(err)
	}
	if quantity != 3 || stock != 7 || requests != 1 {
		t.Fatalf("quantity=%d stock=%d requests=%d", quantity, stock, requests)
	}
}

func TestMerchantEdit_WaitlistedItemsKeepExistingReservationFlow(t *testing.T) {
	f := seedMerchantEdit(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET waitlisted_quantity=1 WHERE id=$1`, f.item); err != nil {
		t.Fatal(err)
	}
	queued, err := f.service.queueMerchantEdit(cartedit.WithRequestID(t.Context(), uuid.NewString()), MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: 1, ByMerchant: true}, "set")
	if err != nil || queued {
		t.Fatalf("queue took over waitlist lifecycle: %v %v", queued, err)
	}
	var quantity int
	if err := testPool.QueryRow(t.Context(), `SELECT quantity FROM cart_items WHERE id=$1`, f.item).Scan(&quantity); err != nil {
		t.Fatal(err)
	}
	if quantity != 2 {
		t.Fatal("fallback changed line")
	}
}

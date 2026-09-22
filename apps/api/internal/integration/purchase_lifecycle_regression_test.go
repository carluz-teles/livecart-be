//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/internal/live"
)

func TestIncidentExpiryMustReturnDatabaseFailure(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	product := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cart := seedHolderCart(t, fx, product, 1)
	setCartExpiresAt(t, cart, time.Now().Add(-time.Minute))
	_, err := testPool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION audit_expiry_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='%s'::uuid THEN RAISE EXCEPTION 'injected production deadlock class' USING ERRCODE='40P01'; END IF; RETURN NEW; END $$; CREATE TRIGGER audit_expiry_failure BEFORE UPDATE OF stock ON products FOR EACH ROW EXECUTE FUNCTION audit_expiry_failure()`, product))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS audit_expiry_failure ON products; DROP FUNCTION IF EXISTS audit_expiry_failure()`)
	})
	err = scaleService().RunScheduledExpiry(t.Context(), cart)
	status := cartStatus(t, cart)
	if err == nil {
		t.Fatalf("lost retry: handler returned nil after database error; cart status=%s", status)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" || status == "expired" {
		t.Fatalf("failed expiry was not rolled back with retryable cause: status=%s err=%v", status, err)
	}
	if _, err := testPool.Exec(t.Context(), `DROP TRIGGER audit_expiry_failure ON products; DROP FUNCTION audit_expiry_failure()`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := scaleService().RunScheduledExpiry(t.Context(), cart); err != nil {
			t.Fatal(err)
		}
	}
	if cartStatus(t, cart) != "expired" || productStock(t, product) != 1 {
		t.Fatal("retry must release stock exactly once")
	}
}

func TestIncidentJoinedChildMustNotBeConfirmedWithoutERPProof(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_state='open',external_order_id='REMOTE-HOST' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	var child string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,payment_status,joined_to_cart_id,purchase_closed,erp_order_state) SELECT event_id,store_id,'audit-child','audit-child','audit-'||id::text,short_id+100000,'checkout','pending',id,true,'none' FROM carts WHERE id=$1 RETURNING id::text`, fx.cartID).Scan(&child); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price,waitlisted_quantity,erp_confirmed_quantity,erp_pending_since) VALUES($1,$2,1,1000,0,0,now())`, child, fx.productID); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.ConfirmarItemNoERP(t.Context(), child, fx.productID); !errors.Is(err, live.ErrCommentERPPending) {
		t.Fatal(err)
	}
	var pending bool
	var confirmed int
	if err := testPool.QueryRow(t.Context(), `SELECT erp_pending_since IS NOT NULL,erp_confirmed_quantity FROM cart_items WHERE cart_id=$1`, child).Scan(&pending, &confirmed); err != nil {
		t.Fatal(err)
	}
	if !pending || confirmed != 0 {
		t.Fatalf("unproven ERP acknowledgement on closed joined child: pending=%v confirmed=%d", pending, confirmed)
	}
}

func TestIncidentReflectionMustNotDuplicateJoinedItems(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	run(`UPDATE carts SET erp_order_state='open',external_order_id='1',erp_order_status='aberto' WHERE id=$1`, fx.cartID)
	var child string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,payment_status,joined_to_cart_id,purchase_closed,erp_order_state) SELECT event_id,store_id,'audit-reflection-child','audit-reflection-child','audit-reflection-'||id::text,short_id+100000,'checkout','pending',id,true,'none' FROM carts WHERE id=$1 RETURNING id::text`, fx.cartID).Scan(&child); err != nil {
		t.Fatal(err)
	}
	run(`INSERT INTO cart_items(cart_id,product_id,quantity,unit_price,waitlisted_quantity) VALUES($1,$2,1,1000,0)`, child, fx.productID)
	provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: "local-fixture"}, Logger: zap.NewNop(), StoreID: fx.storeID, IntegrationID: "audit-reflection"})
	if err != nil {
		t.Fatal(err)
	}
	provider.HTTPClient.Transport = invoicedTinyReadTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/pedidos/1") {
			return nil, fmt.Errorf("unexpected remote write")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":1,"idNotaFiscal":0,"itens":[{"produto":{"id":123},"quantidade":2,"valorUnitario":10}]}`)), Request: r}, nil
	})
	repo := tinyCheckoutProductionRepository(t)
	svc := &Service{repo: repo, logger: zap.NewNop()}
	flow := erp.NewService(erpRepoAdapter{repo}, &invoicedTinyCollaborator{Service: svc, provider: provider}, zap.NewNop())
	flow.SetCartSyncCollaborators(svc)
	if _, err = flow.SyncCartFromERPOrder(t.Context(), fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	var total int
	if err = testPool.QueryRow(t.Context(), `SELECT SUM(ci.quantity-ci.waitlisted_quantity)::int FROM cart_items ci JOIN carts c ON c.id=ci.cart_id WHERE COALESCE(c.joined_to_cart_id,c.id)=$1`, fx.cartID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("reflection duplicated joined purchase: ERP=2; host+child=%d", total)
	}
}

func seedIncidentClosedChild(t *testing.T) (finFixture, string) {
	t.Helper()
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_state='open',external_order_id='audit-host' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	var child string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,payment_status,joined_to_cart_id,purchase_closed,erp_order_state,expires_at) SELECT event_id,store_id,'audit-child-grid','audit-child-grid','audit-grid-'||id::text,short_id+100000,'checkout','pending',id,true,'none',now()-interval '1 minute' FROM carts WHERE id=$1 RETURNING id::text`, fx.cartID).Scan(&child); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price,waitlisted_quantity) VALUES($1,$2,1,1000,0)`, child, fx.productID); err != nil {
		t.Fatal(err)
	}
	return fx, child
}

func TestIncidentChildGridMustResolveHost(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	hostRows, err := testRepo.ListCartGridItems(t.Context(), fx.cartID)
	if err != nil {
		t.Fatal(err)
	}
	childRows, err := testRepo.ListCartGridItems(t.Context(), child)
	if err != nil {
		t.Fatal(err)
	}
	if len(hostRows) == 0 {
		t.Fatal("invalid fixture")
	}
	if len(childRows) != len(hostRows) {
		t.Fatalf("joined child reads empty order grid: host lines=%d child lines=%d", len(hostRows), len(childRows))
	}
}

func TestIncidentClosedChildMustNotReleasePaidHostStock(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	before := productStock(t, fx.productID)
	result, err := testRepo.ExpireCartAndReleaseStock(t.Context(), child, fx.storeID)
	if err != nil {
		t.Fatal(err)
	}
	after := productStock(t, fx.productID)
	if result.Eligible || after != before {
		t.Fatalf("paid joined purchase released inventory on child deadline: eligible=%v stock before=%d after=%d", result.Eligible, before, after)
	}
}

func TestIncidentExpiredChildMustNotCancelPaidHostOrder(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	if _, err := testRepo.ExpireCartAndReleaseStock(t.Context(), child, fx.storeID); err != nil {
		t.Fatal(err)
	}
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	err := svc.ERP().OnCartExpired(t.Context(), child, fx.storeID)
	if fake.count("Situacao:audit-host:2") > 0 {
		t.Fatalf("child expiration called ERP cancellation on already paid host: err=%v calls=%v", err, fake.calls)
	}
}

func TestJoinedPurchaseSharesFinalisationLockAndProtectsManualCancellation(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	release, ok, err := testRepo.AcquireCartFinalisationLock(t.Context(), fx.cartID)
	if err != nil || !ok {
		t.Fatalf("owner lock: %v", err)
	}
	defer release()
	_, ok, err = testRepo.AcquireCartFinalisationLock(t.Context(), child)
	if err != nil || ok {
		t.Fatalf("child bypassed owner lock: acquired=%v err=%v", ok, err)
	}
	before := productStock(t, fx.productID)
	result, err := testRepo.CancelCartAndReleaseStock(t.Context(), child, fx.storeID)
	if err != nil || result.Eligible || productStock(t, fx.productID) != before {
		t.Fatalf("manual child cancellation released paid purchase: %+v %v", result, err)
	}
}

func TestRecentExpiryRecoveryPreservesHistoricalAndProtectedPurchases(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	product := seedSoldOutProductWithQueue(t, fx, 0, 0)
	due := seedHolderCart(t, fx, product, 1)
	old := seedHolderCart(t, fx, product, 1)
	vip := seedHolderCart(t, fx, product, 1)
	invoiced := seedHolderCart(t, fx, product, 1)
	setCartExpiresAt(t, due, time.Now().Add(-23*time.Hour))
	setCartExpiresAt(t, old, time.Now().Add(-48*time.Hour))
	setCartExpiresAt(t, vip, time.Now().Add(-23*time.Hour))
	setCartExpiresAt(t, invoiced, time.Now().Add(-23*time.Hour))
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET never_expires=true WHERE id=$1`, vip); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status='faturado' WHERE id=$1`, invoiced); err != nil {
		t.Fatal(err)
	}
	scaleService().RecoverRecentCartExpiries(t.Context())
	if cartStatus(t, due) != "expired" {
		t.Fatal("recent lost expiry was not recovered")
	}
	for _, id := range []string{old, vip, invoiced} {
		if cartStatus(t, id) == "expired" {
			t.Fatalf("protected/historical cart expired: %s", id)
		}
	}
}

func TestTinyCancellationConfirmsMissingSaleBeforeCompleting(t *testing.T) {
	requireDB(t)
	for _, status := range []int{404, 200, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fx := seedPaidCart(t, 1, 0)
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='pending',purchase_closed=false,paid_amount_cents=0,erp_order_state='open',external_order_id='1' WHERE id=$1`, fx.cartID); err != nil {
				t.Fatal(err)
			}
			provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: "fixture"}, Logger: zap.NewNop(), StoreID: fx.storeID, IntegrationID: "cancel-missing"})
			if err != nil {
				t.Fatal(err)
			}
			puts, gets := 0, 0
			provider.HTTPClient.Transport = invoicedTinyReadTransport(func(r *http.Request) (*http.Response, error) {
				code := status
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pedidos/1/situacao") {
					puts++
					code = 404
				} else if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pedidos/1") {
					gets++
				} else {
					return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"situacao":0}`)), Request: r}, nil
			})
			repo := tinyCheckoutProductionRepository(t)
			svc := &Service{repo: repo, logger: zap.NewNop()}
			flow := erp.NewService(erpRepoAdapter{repo}, &invoicedTinyCollaborator{Service: svc, provider: provider}, zap.NewNop())
			err = flow.CancelERPOrderForCart(t.Context(), fx.cartID, fx.storeID)
			if (err == nil) != (status == 404) || puts != 1 || gets != 1 {
				t.Fatalf("status=%d err=%v PUT=%d GET=%d", status, err, puts, gets)
			}
			state, err := repo.GetCartERPOrderState(t.Context(), fx.cartID)
			if err != nil {
				t.Fatal(err)
			}
			expected := "open"
			if status == 404 {
				expected = "cancelled"
			}
			if state.State != expected || state.ExternalOrderID != "1" {
				t.Fatalf("unsafe cancellation state: %+v", state)
			}
		})
	}
}

func TestQuantityAloneCannotAcknowledgeDifferentERPPrice(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET external_id='123' WHERE id=$1`, fx.productID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='1',erp_order_state='open' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=0 WHERE cart_id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.ConfirmERPGrid(t.Context(), fx.cartID, []providers.ERPOrderItem{{ProductID: "123", Quantity: 1, UnitPrice: 999}}); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.ConfirmarItemNoERP(t.Context(), fx.cartID, fx.productID); !errors.Is(err, live.ErrCommentERPPending) {
		t.Fatal(err)
	}
	var pending bool
	if err := testPool.QueryRow(t.Context(), `SELECT erp_pending_since IS NOT NULL FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&pending); err != nil || !pending {
		t.Fatalf("price mismatch was acknowledged: pending=%v err=%v", pending, err)
	}
}

func TestTinyInvoiceReadDoesNotUnlockMutation(t *testing.T) {
	provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: "fixture"}, Logger: zap.NewNop(), StoreID: "local", IntegrationID: "invoice-read"})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	provider.HTTPClient.Transport = invoicedTinyReadTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, fmt.Errorf("unexpected ERP write")
		}
		reads++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":1,"idNotaFiscal":99,"itens":[{"produto":{"id":123},"quantidade":1,"valorUnitario":10}]}`)), Request: r}, nil
	})
	items, err := provider.GetOrderItemsForReflection(t.Context(), "1")
	if err != nil || len(items) != 1 || items[0].UnitPrice != 1000 {
		t.Fatalf("read-only invoice: %v %v", items, err)
	}
	if _, err = provider.GetOrderItems(t.Context(), "1"); !errors.Is(err, providers.ErrPedidoComNotaFiscal) {
		t.Fatalf("mutation preflight lost invoice guard: %v", err)
	}
	if reads != 2 {
		t.Fatalf("unexpected reads: %d", reads)
	}
}

func TestJoinedChildCreationUsesOwnerBindingAndCompleteGrid(t *testing.T) {
	fx, child := seedIncidentClosedChild(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='pending',paid_amount_cents=0,purchase_closed=false,erp_order_state='none',external_order_id=NULL WHERE id=ANY($1::uuid[])`, []string{fx.cartID, child}); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `DELETE FROM cart_items WHERE cart_id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	if err := svc.EnsureERPOrderForCart(t.Context(), child, fx.storeID); err != nil {
		t.Fatal(err)
	}
	state, external, _, _ := cartERPState(t, fx.cartID)
	_, childExternal, _, _ := cartERPState(t, child)
	if state != "open" || external == "" || childExternal != "" || fake.count("CreateOrder") != 1 || len(fake.ultimaGrade) != 1 || fake.ultimaGrade[0].Quantity != 1 {
		t.Fatalf("wrong joined creation: state=%s owner=%s child=%s grid=%v calls=%v", state, external, childExternal, fake.ultimaGrade, fake.calls)
	}
	if err := svc.EnsureERPOrderForCart(t.Context(), fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if fake.count("CreateOrder") != 1 {
		t.Fatal("owner entry point duplicated child-triggered order")
	}
}

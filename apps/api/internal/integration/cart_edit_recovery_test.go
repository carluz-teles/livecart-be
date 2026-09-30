package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
)

type cancellationBarrier struct {
	*scriptedERP
	entered chan struct{}
	resume  chan struct{}
}

func (f *cancellationBarrier) SetOrderSituacao(ctx context.Context, _ string, _ int) error {
	close(f.entered)
	select {
	case <-f.resume:
		return errors.New("cancellation refused")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCartEditCancellationVerificationDoesNotTrustLocalClaim(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	fake := &cancellationBarrier{scriptedERP: newScriptedERP(), entered: make(chan struct{}), resume: make(chan struct{})}
	svc := newFinalisationService(fake)
	if _, err := testPool.Exec(ctx, `UPDATE carts SET payment_status='unpaid',paid_at=NULL,erp_order_state='open',external_order_id='cancel-test',status='cancelled' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	var integration string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM integrations WHERE store_id=$1 AND status='active'`, fx.storeID).Scan(&integration); err != nil {
		t.Fatal(err)
	}
	ctx = erp.WithExpectedIntegration(ctx, integration)
	done := make(chan error, 1)
	go func() { done <- svc.CancelERPOrderForCart(ctx, fx.cartID, fx.storeID) }()
	select {
	case <-fake.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	err := svc.VerifyERPOrderCancelled(ctx, fx.cartID, fx.storeID)
	close(fake.resume)
	if !errors.Is(err, erp.ErrCartBusy) {
		t.Errorf("accepted provisional cancellation: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("expected provider refusal")
	}
	if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_order_state='cancelled' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyERPOrderCancelled(ctx, fx.cartID, fx.storeID); !errors.Is(err, erp.ErrCancellationUnconfirmed) {
		t.Fatalf("local cancelled but remote open accepted: %v", err)
	}
	fake.mu.Lock()
	fake.situacoes["cancel-test"] = providers.SituacaoCancelada
	fake.mu.Unlock()
	if err := svc.VerifyERPOrderCancelled(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM integrations WHERE id=$1`, integration); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'erp','tiny','active')`, fx.storeID); err != nil {
		t.Fatal(err)
	}
	before := fake.count("GetSituacao:")
	if err := svc.VerifyERPOrderCancelled(ctx, fx.cartID, fx.storeID); err == nil {
		t.Fatal("verified against replacement account")
	}
	if fake.count("GetSituacao:") != before {
		t.Fatal("read replacement ERP")
	}
}

func seedRecoveryEdit(t *testing.T, fx finFixture) string {
	t.Helper()
	var integration string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1 AND status='active'`, fx.storeID).Scan(&integration); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_erp_edits(cart_id,revision) VALUES($1,1)`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_erp_edit_requests(id,cart_id,revision,request,product_id,retained_quantity)
 VALUES(gen_random_uuid(),$1,1,jsonb_build_object('_execution',jsonb_build_object('version',1,'remote',true,'integrationId',$3::text,'provider','tiny')),$2,2)`, fx.cartID, fx.productID, integration); err != nil {
		t.Fatal(err)
	}
	return integration
}

func TestCartEditOperationSweepKeepsOriginalAccountAndLease(t *testing.T) {
	for _, state := range []string{"mutating", "converting_new", "converting_bound"} {
		t.Run(state, func(t *testing.T) {
			requireDB(t)
			fx := seedPaidCart(t, 2, 0)
			t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM carts WHERE id=$1`, fx.cartID) })
			fake := newScriptedERP()
			svc := newFinalisationService(fake)
			svc.erpProviderFactory = func(_ context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
				if integration.StoreID != fx.storeID {
					return nil, errors.New("outside test account")
				}
				return fake, nil
			}
			ctx := t.Context()
			if state != "converting_new" {
				if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
					t.Fatal(err)
				}
			}
			original := seedRecoveryEdit(t, fx)
			operation := "converting"
			if state == "mutating" {
				operation = state
			}
			if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_order_state=$2,erp_op_resting_state='open',erp_op_started_at=now()-interval '4 minutes' WHERE id=$1`, fx.cartID, operation); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.calls = nil
			fake.mu.Unlock()
			if _, err := testPool.Exec(ctx, `UPDATE cart_erp_edits SET lease_owner=gen_random_uuid(),lease_until=now()+interval '3 minutes' WHERE cart_id=$1`, fx.cartID); err != nil {
				t.Fatal(err)
			}
			svc.RunERPOrderOpsSweep(ctx)
			if got, _, _, _ := cartERPState(t, fx.cartID); got != operation {
				t.Fatalf("sweep stole active lease: %s", got)
			}
			if len(fake.callsWithPrefix("")) != 0 {
				t.Fatal("active worker lease ignored")
			}
			if _, err := testPool.Exec(ctx, `UPDATE cart_erp_edits SET lease_until=now()-interval '1 second' WHERE cart_id=$1`, fx.cartID); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `DELETE FROM integrations WHERE id=$1`, original); err != nil {
				t.Fatal(err)
			}
			var replacement string
			if err := testPool.QueryRow(ctx, `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'erp','tiny','active') RETURNING id::text`, fx.storeID).Scan(&replacement); err != nil {
				t.Fatal(err)
			}
			svc.RunERPOrderOpsSweep(ctx)
			if got, _, _, _ := cartERPState(t, fx.cartID); got != operation {
				t.Fatalf("replacement changed state to %s", got)
			}
			if len(fake.callsWithPrefix("")) != 0 {
				t.Fatal("sweep called replacement account")
			}
			if _, err := testPool.Exec(ctx, `DELETE FROM integrations WHERE id=$1`, replacement); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `INSERT INTO integrations(id,store_id,type,provider,status) VALUES($1,$2,'erp','tiny','active')`, original, fx.storeID); err != nil {
				t.Fatal(err)
			}
			svc.RunERPOrderOpsSweep(ctx)
			if got, _, _, _ := cartERPState(t, fx.cartID); got != "open" {
				t.Fatalf("original account failed to recover: %s", got)
			}
			status, err := cartedit.Read(ctx, testPool, fx.cartID)
			if err != nil || !status.Pending || status.Processing {
				t.Fatalf("sweep settled edit or kept lease: %+v %v", status, err)
			}
			var retained int
			if err := testPool.QueryRow(ctx, `SELECT SUM(retained_quantity) FROM cart_erp_edit_requests WHERE cart_id=$1`, fx.cartID).Scan(&retained); err != nil {
				t.Fatal(err)
			}
			if retained != 2 {
				t.Fatalf("sweep released retained stock: %d", retained)
			}
		})
	}
}

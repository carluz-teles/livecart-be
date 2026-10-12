//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
)

func seedInvoiceHistory(t *testing.T) finFixture {
	t.Helper()
	fx := seedPaidCart(t, 1, 0)
	invoiceHistoryExec(t, `DELETE FROM orders WHERE cart_id=$1`, fx.cartID)
	invoiceHistoryExec(t, `UPDATE carts SET created_at='2026-09-01T12:00:00Z',paid_at='2026-09-10T12:00:00Z',
        payment_status='paid',paid_amount_cents=1000,checkout_id='erp-history-order',
        external_order_id='history-order',erp_order_state='open',erp_order_status='faturado'
        WHERE id=$1`, fx.cartID)
	invoiceHistoryExec(t, `UPDATE live_events SET status='ended',ends_at='2026-09-08T02:59:00Z'
        WHERE id=(SELECT event_id FROM carts WHERE id=$1)`, fx.cartID)
	invoiceHistoryExec(t, `INSERT INTO cart_payments
        (cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at,created_at)
        VALUES($1,1000,1000,'erp_manual','erp-history-order','2026-09-10T12:00:00Z','2026-09-10T12:00:00Z')`, fx.cartID)
	invoiceHistoryExec(t, `UPDATE cart_item_price_lots SET created_at='2026-09-01T12:00:00Z'
        WHERE cart_item_id IN (SELECT id FROM cart_items WHERE cart_id=$1)`, fx.cartID)
	return fx
}

func invoiceHistoryExec(t *testing.T, query, cartID string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), query, cartID); err != nil {
		t.Fatal(err)
	}
}

func TestInvoiceHistoryRequiresLegacyProvenanceAndNoCurrentWorkflow(t *testing.T) {
	requireDB(t)
	cases := []struct {
		name   string
		query  string
		legacy bool
	}{
		{name: "old external payment without operational work", legacy: true},
		{name: "old active cart with manual ERP payment", legacy: true,
			query: `UPDATE carts SET status='active',erp_order_status='preparando_envio' WHERE id=$1`},
		{name: "historical waitlist does not authorize new fulfillment", legacy: true,
			query: `UPDATE cart_items SET waitlisted_quantity=quantity WHERE cart_id=$1`},
		{name: "empty address is not logistics", legacy: true,
			query: `UPDATE carts SET shipping_address='{}' WHERE id=$1`},
		{name: "old completed edit remains history", legacy: true,
			query: `INSERT INTO cart_erp_edits(cart_id,revision,synced_revision,queued_at)
                VALUES($1,1,1,'2026-09-10T12:00:00Z')`},
		{name: "operation timestamp retained in resting state is not current work", legacy: true,
			query: `UPDATE carts SET erp_op_started_at=now() WHERE id=$1`},
		{name: "migration materializes the original old lot with a new timestamp", legacy: true,
			query: `UPDATE cart_item_price_lots SET created_at=now(),attribution_from_log=true
                WHERE cart_item_id IN (SELECT id FROM cart_items WHERE cart_id=$1)`},
		{name: "new cart never becomes exempt when event is old",
			query: `UPDATE carts SET created_at='2026-10-01T03:00:00Z' WHERE id=$1`},
		{name: "recent payment on an old cart",
			query: `UPDATE carts SET paid_at='2026-10-05T12:00:00Z' WHERE id=$1`},
		{name: "unknown payment date",
			query: `UPDATE carts SET paid_at=NULL WHERE id=$1`},
		{name: "recent ledger payment despite old cart date",
			query: `UPDATE cart_payments SET paid_at='2026-10-05T12:00:00Z' WHERE cart_id=$1`},
		{name: "recently recorded backdated payment",
			query: `UPDATE cart_payments SET created_at='2026-10-05T12:00:00Z' WHERE cart_id=$1`},
		{name: "payment precisely on fixed boundary",
			query: `UPDATE cart_payments SET paid_at='2026-10-01T03:00:00Z' WHERE cart_id=$1`},
		{name: "checkout payment must materialize order",
			query: `UPDATE cart_payments SET method='pix',checkout_id='gateway-charge' WHERE cart_id=$1`},
		{name: "ERP payment belongs to different binding",
			query: `UPDATE cart_payments SET checkout_id='erp-other-order' WHERE cart_id=$1`},
		{name: "inconsistent ledger keeps diagnostic",
			query: `UPDATE cart_payments SET amount_cents=900 WHERE cart_id=$1`},
		{name: "missing ledger is not historical evidence",
			query: `DELETE FROM cart_payments WHERE cart_id=$1`},
		{name: "unpaid old cart",
			query: `UPDATE carts SET payment_status='pending' WHERE id=$1`},
		{name: "active event",
			query: `UPDATE live_events SET status='active' WHERE id=(SELECT event_id FROM carts WHERE id=$1)`},
		{name: "recently ended event",
			query: `UPDATE live_events SET ends_at='2026-10-10T02:59:00Z' WHERE id=(SELECT event_id FROM carts WHERE id=$1)`},
		{name: "existing materialized order continues invoice sync",
			query: `INSERT INTO orders(cart_id,short_id,store_id,event_id)
                SELECT id,short_id,store_id,event_id FROM carts WHERE id=$1`},
		{name: "ERP order reopened despite historical payment",
			query: `UPDATE carts SET erp_order_status='aberto' WHERE id=$1`},
		{name: "current ERP mutation",
			query: `UPDATE carts SET erp_order_state='mutating',erp_op_started_at=now() WHERE id=$1`},
		{name: "recent cancellation reversal",
			query: `UPDATE carts SET cancellation_reverted_at=now() WHERE id=$1`},
		{name: "pending item",
			query: `UPDATE cart_items SET erp_pending_since=now() WHERE cart_id=$1`},
		{name: "pending ERP item retry",
			query: `UPDATE carts SET erp_items_retry_at=now() WHERE id=$1`},
		{name: "new item addition",
			query: `INSERT INTO cart_item_events(cart_id,product_id,quantity,unit_price)
                SELECT cart_id,product_id,1,unit_price FROM cart_items WHERE cart_id=$1`},
		{name: "ERP reflection without local sales activity does not require an invoice", legacy: true,
			query: `UPDATE cart_items SET quantity=quantity+1 WHERE cart_id=$1`},
		{name: "recent completed quantity edit",
			query: `INSERT INTO cart_mutations(cart_id,product_id,mutation_type,quantity_before,quantity_after,unit_price)
                SELECT cart_id,product_id,'quantity_decreased',2,1,unit_price FROM cart_items WHERE cart_id=$1`},
		{name: "old pending merchant edit",
			query: `INSERT INTO cart_erp_edits(cart_id,revision,synced_revision,queued_at)
                VALUES($1,1,0,'2026-09-10T12:00:00Z')`},
		{name: "recent completed merchant edit",
			query: `INSERT INTO cart_erp_edits(cart_id,revision,synced_revision) VALUES($1,1,1)`},
		{name: "unfinished historical comment still requires reconciliation",
			query: `WITH work AS (
                INSERT INTO live_comment_work(platform_comment_id,payload) VALUES($1::text,'{}')
                RETURNING platform_comment_id
            ) INSERT INTO cart_item_events(cart_id,product_id,quantity,unit_price,created_at,platform_comment_id)
                SELECT ci.cart_id,ci.product_id,1,ci.unit_price,'2026-09-01T12:00:00Z',w.platform_comment_id
                FROM cart_items ci CROSS JOIN work w WHERE ci.cart_id=$1::uuid`},
		{name: "open financial review regardless of age",
			query: `INSERT INTO erp_payment_reviews(cart_id,external_order_id,reason,detected_at,checked_at)
                VALUES($1,'history-order','installments_unverified','2026-09-10T12:00:00Z','2026-09-10T12:00:00Z')`},
		{name: "recently resolved review remains current workflow",
			query: `INSERT INTO erp_payment_reviews(cart_id,external_order_id,reason,resolved_at)
                VALUES($1,'history-order','installments_unverified',now())`},
		{name: "checkout payment review",
			query: `UPDATE carts SET payment_review_required=true WHERE id=$1`},
		{name: "shipping address requires local invoice workflow",
			query: `UPDATE carts SET shipping_address='{"city":"Teste"}' WHERE id=$1`},
		{name: "shipping selected",
			query: `UPDATE carts SET shipping_service_id='shipping-service' WHERE id=$1`},
		{name: "shipment without an address still requires invoice",
			query: `INSERT INTO shipments(cart_id,store_id,provider,provider_order_id)
                SELECT id,store_id,'test-carrier',id::text FROM carts WHERE id=$1`},
		{name: "gateway attempt prevents legacy exemption",
			query: `INSERT INTO payment_attempts(cart_id,integration_id,provider,amount_cents,cart_fingerprint)
                SELECT c.id,i.id,'test-gateway',1000,'fixture' FROM carts c
                JOIN integrations i ON i.store_id=c.store_id WHERE c.id=$1`},
		{name: "current paid checkout reconciliation",
			query: `INSERT INTO tiny_checkout_operations(id,cart_id,integration_id,source_order_id,progress)
                SELECT gen_random_uuid(),c.id,i.id,'history-order','{}' FROM carts c
                JOIN integrations i ON i.store_id=c.store_id WHERE c.id=$1`},
		{name: "nonexpiring cart is still paid history without current workflow", legacy: true,
			query: `UPDATE carts SET never_expires=true WHERE id=$1`},
		{name: "joined child requires group workflow",
			query: `INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,joined_to_cart_id)
                SELECT event_id,store_id,platform_user_id||'-child',platform_handle,token||'-child',short_id+1,'active',id
                FROM carts WHERE id=$1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedInvoiceHistory(t)
			if tc.query != "" {
				invoiceHistoryExec(t, tc.query, fx.cartID)
			}
			got, err := testRepo.IsLegacyCartWithoutInvoiceWorkflow(t.Context(), fx.storeID, fx.cartID, "history-order")
			if err != nil || got != tc.legacy {
				t.Fatalf("legacy=%v want=%v err=%v", got, tc.legacy, err)
			}
		})
	}
}

func TestInvoiceHistoryRequiresExactStoreAndExternalBinding(t *testing.T) {
	requireDB(t)
	fx := seedInvoiceHistory(t)
	for _, tc := range []struct {
		name, storeID, cartID, externalID string
	}{
		{name: "different store", storeID: uuid.NewString(), cartID: fx.cartID, externalID: "history-order"},
		{name: "different cart", storeID: fx.storeID, cartID: uuid.NewString(), externalID: "history-order"},
		{name: "different ERP order", storeID: fx.storeID, cartID: fx.cartID, externalID: "new-order"},
		{name: "unbound ERP order", storeID: fx.storeID, cartID: fx.cartID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, err := testRepo.IsLegacyCartWithoutInvoiceWorkflow(t.Context(), tc.storeID, tc.cartID, tc.externalID)
			if err != nil || legacy {
				t.Fatalf("wrong binding classified as history: legacy=%v err=%v", legacy, err)
			}
		})
	}
}

// Nil embedded methods fail the test if classification ever reaches creation,
// payment, invoice fetch, or another provider operation.
type invoiceHistoryUntouchedProvider struct{ providers.ERPProvider }

func TestInvoiceHistoryWebhookLeavesDatabaseAndERPUnchanged(t *testing.T) {
	requireDB(t)
	fx := seedInvoiceHistory(t)
	read := func() string {
		t.Helper()
		var snapshot string
		err := testPool.QueryRow(t.Context(), `SELECT jsonb_build_object(
            'cart',to_jsonb(c),
            'payments',(SELECT jsonb_agg(to_jsonb(p)) FROM cart_payments p WHERE p.cart_id=c.id),
            'items',(SELECT jsonb_agg(to_jsonb(i)) FROM cart_items i WHERE i.cart_id=c.id),
            'orders',(SELECT count(*) FROM orders o WHERE o.cart_id=c.id),
            'shipments',(SELECT count(*) FROM shipments s WHERE s.cart_id=c.id),
            'outbox',(SELECT count(*) FROM event_outbox e WHERE e.payload->>'cart_id'=c.id::text)
        )::text FROM carts c WHERE c.id=$1`, fx.cartID).Scan(&snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := read()
	core, observed := observer.New(zap.InfoLevel)
	flow := erp.NewService(
		erpRepoAdapter{testRepo},
		&invoicedTinyCollaborator{provider: &invoiceHistoryUntouchedProvider{}},
		zap.New(core),
	)
	for range 2 {
		state, err := flow.SyncCartInvoiceByExternalOrder(t.Context(), fx.storeID, "history-order", "invoice-id")
		if err != nil || state != nil {
			t.Fatalf("historical webhook was not skipped: state=%+v err=%v", state, err)
		}
	}
	if before != read() {
		t.Fatal("history classification changed the cart, payment, order, shipment or emitted an event")
	}
	if observed.Len() != 2 || observed.FilterLevelExact(zap.InfoLevel).Len() != 2 {
		t.Fatalf("historical webhook should produce information only: %+v", observed.All())
	}
}

func TestInvoiceHistoryCancelledReadIsNotClassifiedAsLegacy(t *testing.T) {
	requireDB(t)
	fx := seedInvoiceHistory(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	legacy, err := testRepo.IsLegacyCartWithoutInvoiceWorkflow(ctx, fx.storeID, fx.cartID, "history-order")
	if legacy || err == nil {
		t.Fatalf("failed read was swallowed: legacy=%v err=%v", legacy, err)
	}
}

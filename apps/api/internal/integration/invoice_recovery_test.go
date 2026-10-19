//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	orderlisteners "livecart/apps/api/internal/order/listeners"
)

func seedInvoiceObservation(t *testing.T) (finFixture, UpsertCartERPInvoiceParams) {
	t.Helper()
	fx := seedPaidCart(t, 1, 0)
	approvalExec(t, `UPDATE carts SET external_order_id='invoice-order' WHERE id=$1`, fx.cartID)
	p := UpsertCartERPInvoiceParams{
		CartID: fx.cartID, StoreID: fx.storeID, ExternalOrderID: "invoice-order",
		InvoiceID: "invoice-1", InvoiceKey: "invoice-key", InvoiceStatus: "authorized",
		ObservedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1`, fx.storeID).
		Scan(&p.IntegrationID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM cart_invoice_observations WHERE cart_id=$1`, fx.cartID); err != nil {
			t.Error(err)
		}
	})
	return fx, p
}

func invoiceStatus(t *testing.T, cartID string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(t.Context(), `SELECT COALESCE(op.invoice_status,'') FROM order_payments op
 JOIN orders o ON o.id=op.order_id WHERE o.cart_id=$1`, cartID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestInvoiceBeforeOrderSurvivesRestartAndMaterialisation(t *testing.T) {
	requireDB(t)
	fx, p := seedInvoiceObservation(t)
	approvalExec(t, `DELETE FROM orders WHERE cart_id=$1`, fx.cartID)
	before := paymentLedgerSnapshot(t, fx.cartID)
	count, err := testRepo.UpsertCartERPInvoice(t.Context(), p)
	if err != nil || count != 0 {
		t.Fatalf("retain invoice: rows=%d err=%v", count, err)
	}
	// A new service has no in-memory knowledge of the received invoice.
	scaleService().RecoverCartInvoices(t.Context())
	if after := paymentLedgerSnapshot(t, fx.cartID); after != before {
		t.Fatal("invoice recovery created a sale, recorded money or emitted an event")
	}
	listener := orderlisteners.New(testPool, testRepo.queries, zap.NewNop())
	if err := listener.OnCartPaid(t.Context(), fx.cartID, fx.storeID, 1000, nil); err != nil {
		t.Fatal(err)
	}
	before = paymentLedgerSnapshot(t, fx.cartID)
	for range 2 {
		scaleService().RecoverCartInvoices(t.Context())
	}
	if status := invoiceStatus(t, fx.cartID); status != "authorized" {
		t.Fatalf("recovered status=%q", status)
	}
	if after := paymentLedgerSnapshot(t, fx.cartID); after != before {
		t.Fatal("metadata recovery changed commercial records")
	}
	var applied bool
	if err := testPool.QueryRow(t.Context(), `SELECT applied_at IS NOT NULL FROM cart_invoice_observations WHERE cart_id=$1`, fx.cartID).
		Scan(&applied); err != nil || !applied {
		t.Fatalf("observation not completed: %v %v", applied, err)
	}
}

func TestInvoiceRejectsChangedBindings(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct{ name, query string }{
		{"replacement order", `UPDATE carts SET external_order_id='replacement' WHERE id=$1`},
		{"disabled integration", `UPDATE integrations SET status='disconnected' WHERE store_id=(SELECT store_id FROM carts WHERE id=$1)`},
		{"different store", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, p := seedInvoiceObservation(t)
			if tc.query == "" {
				other := seedPaidCart(t, 1, 0)
				p.StoreID = other.storeID
			} else {
				approvalExec(t, tc.query, fx.cartID)
			}
			before := paymentLedgerSnapshot(t, fx.cartID)
			if _, err := testRepo.UpsertCartERPInvoice(t.Context(), p); !errors.Is(err, erp.ErrCartBusy) {
				t.Fatalf("changed binding accepted: %v", err)
			}
			scaleService().RecoverCartInvoices(t.Context())
			if invoiceStatus(t, fx.cartID) != "" || paymentLedgerSnapshot(t, fx.cartID) != before {
				t.Fatal("changed binding modified the order")
			}
		})
	}
}

func TestInvoiceConcurrentOlderObservationCannotUndoCancellation(t *testing.T) {
	requireDB(t)
	fx, old := seedInvoiceObservation(t)
	newer := old
	newer.ObservedAt = old.ObservedAt.Add(time.Second)
	newer.InvoiceStatus = "cancelled"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, p := range []UpsertCartERPInvoiceParams{old, newer} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := testRepo.UpsertCartERPInvoice(t.Context(), p)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, erp.ErrCartBusy) {
			t.Fatal(err)
		}
	}
	if _, err := testRepo.UpsertCartERPInvoice(t.Context(), old); !errors.Is(err, erp.ErrCartBusy) {
		t.Fatalf("stale invoice accepted: %v", err)
	}
	scaleService().RecoverCartInvoices(t.Context())
	if status := invoiceStatus(t, fx.cartID); status != "cancelled" {
		t.Fatalf("status=%q", status)
	}
}

func TestRetainedInvoiceIsNotAppliedAfterOrderReplacement(t *testing.T) {
	requireDB(t)
	fx, p := seedInvoiceObservation(t)
	approvalExec(t, `DELETE FROM order_payments WHERE order_id=(SELECT id FROM orders WHERE cart_id=$1)`, fx.cartID)
	if count, err := testRepo.UpsertCartERPInvoice(t.Context(), p); err != nil || count != 0 {
		t.Fatalf("retain: %d %v", count, err)
	}
	approvalExec(t, `UPDATE carts SET external_order_id='replacement' WHERE id=$1`, fx.cartID)
	approvalExec(t, `INSERT INTO order_payments(order_id) SELECT id FROM orders WHERE cart_id=$1`, fx.cartID)
	scaleService().RecoverCartInvoices(t.Context())
	if invoiceStatus(t, fx.cartID) != "" {
		t.Fatal("old invoice attached to replacement")
	}
}

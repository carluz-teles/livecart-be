//go:build integration

package checkout

import (
	"context"
	"testing"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
)

type blockedEditStockERP struct {
	providers.ERPProvider
	available, reads int
}

func (p *blockedEditStockERP) GetProductStock(context.Context, string) (int, error) {
	p.reads++
	return p.available, nil
}

func TestBlockedEditReconciliationReadsFreshStockInsteadOfCreditingClampedBalance(t *testing.T) {
	f := seedMerchantEdit(t)
	if err := f.service.RemoveCartItemAsMerchant(t.Context(), f.token, f.item); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status='faturado' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	fake := &blockedEditStockERP{available: 1}
	svc := wireCheckoutERP(t, f, fake)
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	status, err := cartedit.Read(t.Context(), testPool, f.cart)
	if err != nil || !status.Pending || !status.Blocked {
		t.Fatalf("invoice did not block edit: %+v %v", status, err)
	}
	var integrationID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1`, f.store).
		Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	command := integration.TinyProductWebhookCommand{
		StoreID: f.store, IntegrationID: integrationID, ProductID: "1234", Kind: "estoque", Revision: 1,
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO erp_stock_sync_state(product_id,requested_revision)
		VALUES($1,1)`, f.product); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessTinyProductWebhook(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 0) // One available minus two retained; the counter clamps at zero.
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status='cancelled',erp_order_state='cancelled'
		WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	if err := cartedit.AssertReady(t.Context(), testPool, f.cart); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 0) // Crediting the two retained units here would exceed the ERP's one.
	repo := integration.NewRepository(testQueries, testPool)
	if err := repo.IncrementProductStock(t.Context(), f.product, 2); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 0) // Other releases must also wait for the fresh read after the journal clears.
	var deferred bool
	if err := testPool.QueryRow(t.Context(), `SELECT deferred_at IS NOT NULL FROM erp_stock_sync_state
		WHERE product_id=$1`, f.product).Scan(&deferred); err != nil || !deferred {
		t.Fatalf("missing durable stock recovery: deferred=%v err=%v", deferred, err)
	}
	// Even a previously completed notification must read again after reconciliation.
	if err := svc.ProcessTinyProductWebhook(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 1)
	f.service.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 1)
	if fake.reads != 2 {
		t.Fatalf("fresh ERP reads=%d, want 2", fake.reads)
	}
}

func TestBlockedEditHoldsJoinedAndDetachedOriginsWithoutDoubleCounting(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "joined"
		if detached {
			name = "detached"
		}
		t.Run(name, func(t *testing.T) {
			f := seedMerchantEdit(t)
			child, token, item := seedEditChild(t, f)
			for _, edit := range []struct{ token, item string }{{token, item}, {f.token, f.item}} {
				if err := f.service.SetCartItemQuantityAsMerchant(t.Context(), edit.token, edit.item, 1); err != nil {
					t.Fatal(err)
				}
			}
			if detached {
				if _, err := testPool.Exec(t.Context(), `UPDATE carts SET joined_to_cart_id=NULL WHERE id=$1`, child); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status='faturado' WHERE id=$1`, f.cart); err != nil {
				t.Fatal(err)
			}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			status, err := cartedit.Read(t.Context(), testPool, f.cart)
			if err != nil || !status.Blocked {
				t.Fatalf("expected reconciliation: %+v %v", status, err)
			}
			repo := integration.NewRepository(testQueries, testPool)
			for _, localReservation := range []bool{false, true} {
				got, err := repo.SumPromisedNotYetReflected(t.Context(), f.store, "tiny", "1234", localReservation)
				if err != nil || got != 4 { // Two held units and two retained removals, counted once each.
					t.Fatalf("local=%v promised=%d want=4 err=%v", localReservation, got, err)
				}
			}
			other := seedMerchantEdit(t) // Same ERP ID, different store.
			got, err := repo.SumPromisedNotYetReflected(t.Context(), other.store, "tiny", "1234", true)
			if err != nil || got != 2 {
				t.Fatalf("hold crossed store boundary: promised=%d err=%v", got, err)
			}
		})
	}
}

func TestBlockedManualProductStillReleasesLocalRetention(t *testing.T) {
	f := seedMerchantEdit(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET external_id=NULL,external_source='manual' WHERE id=$1`,
		f.product); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RemoveCartItemAsMerchant(t.Context(), f.token, f.item); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status='cancelled',erp_order_state='cancelled' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{verify: func(context.Context, string, string) error {
		return erp.ErrCancellationUnconfirmed
	}}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	status, err := cartedit.Read(t.Context(), testPool, f.cart)
	if err != nil || !status.Blocked {
		t.Fatalf("expected blocked cancellation: %+v %v", status, err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 10)
}

func TestBlockedEditCancellationCannotCreditClampedStock(t *testing.T) {
	f := seedMerchantEdit(t)
	if err := f.service.SetCartItemQuantityAsMerchant(t.Context(), f.token, f.item, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status='faturado' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	fake := &blockedEditStockERP{available: 0}
	svc := wireCheckoutERP(t, f, fake)
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	var integrationID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1`, f.store).
		Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessTinyProductWebhook(t.Context(), integration.TinyProductWebhookCommand{
		StoreID: f.store, IntegrationID: integrationID, ProductID: "1234", Kind: "estoque",
	}); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 0)
	repo := integration.NewRepository(testQueries, testPool)
	result, err := repo.CancelCartAndReleaseStock(t.Context(), f.cart, f.store)
	if err != nil || !result.Eligible {
		t.Fatalf("cancel=%+v err=%v", result, err)
	}
	assertEditStock(t, f, 0) // Cancellation must not bypass the blocked journal's stock recovery.
}

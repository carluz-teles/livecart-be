//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

func seedStockPendingEdit(t *testing.T, storeID, productID string) string {
	t.Helper()
	var cartID string
	if err := testPool.QueryRow(t.Context(), `SELECT c.id::text FROM carts c JOIN live_events e ON e.id=c.event_id
		WHERE e.store_id=$1 LIMIT 1`, storeID).Scan(&cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_erp_edits(cart_id,revision) VALUES($1,1)`, cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_erp_edit_requests(id,cart_id,revision,request,product_id,retained_quantity)
		VALUES(gen_random_uuid(),$1,1,'{"operation":"remove","quantity":0}',$2,1)`, cartID, productID); err != nil {
		t.Fatal(err)
	}
	return cartID
}

func TestERPResyncContinuesPastPendingEditWithoutReleasingStock(t *testing.T) {
	reads := map[string]int{}
	svc, row := resyncFixture(t, 7, func(id string) (*providers.ERPProduct, error) {
		reads[id]++
		return syncedProduct(id)
	})
	var productID, externalID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text,external_id FROM products WHERE store_id=$1 ORDER BY id LIMIT 1`, row.StoreID).
		Scan(&productID, &externalID); err != nil {
		t.Fatal(err)
	}
	cartID := seedStockPendingEdit(t, row.StoreID, productID)
	before := estoqueDoProduto(t, productID)
	if _, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := svc.RunERPResync(t.Context(), resyncCommand(t, row)); err != nil {
			t.Fatal(err)
		}
	}
	p := resyncProgressForTest(t, svc, row)
	if p.Done != 7 || p.Succeeded != 6 || p.Failed != 1 || p.Status != "completed_with_errors" {
		t.Fatalf("blocked product held catalog: %+v", p)
	}
	if reads[externalID] != 1 || estoqueDoProduto(t, productID) != before {
		t.Fatalf("repeated blocked reads or released stock: reads=%d stock=%d", reads[externalID], estoqueDoProduto(t, productID))
	}
	var pending, deferred bool
	var name string
	if err := testPool.QueryRow(t.Context(), `SELECT w.revision>w.synced_revision,s.deferred_at IS NOT NULL,p.name
		FROM products p JOIN erp_stock_sync_state s ON s.product_id=p.id JOIN cart_erp_edits w ON w.cart_id=$2
		WHERE p.id=$1`, productID, cartID).Scan(&pending, &deferred, &name); err != nil {
		t.Fatal(err)
	}
	if !pending || !deferred || name != "Updated product" {
		t.Fatalf("lost pending stock or metadata sync: pending=%v deferred=%v name=%q", pending, deferred, name)
	}
}

func TestTinyDeferredStockWebhookRecoversAfterEditReconciliation(t *testing.T) {
	svc, row, productID, externalID := stockConsistencyFixture(t, syncedProduct)
	cartID := seedStockPendingEdit(t, row.StoreID, productID)
	reads := 0
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { reads++; return 3, nil }}, nil
	}})
	command := TinyProductWebhookCommand{StoreID: row.StoreID, IntegrationID: row.ID, ProductID: externalID, Kind: "estoque"}
	for range 2 {
		if err := svc.ProcessTinyProductWebhook(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 0 || estoqueDoProduto(t, productID) != 10 {
		t.Fatal("blocked webhook consumed provider budget or replaced stock")
	}
	var deferred, success bool
	if err := testPool.QueryRow(t.Context(), `SELECT deferred_at IS NOT NULL,last_success_at IS NOT NULL
		FROM erp_stock_sync_state WHERE product_id=$1`, productID).Scan(&deferred, &success); err != nil || !deferred || success {
		t.Fatalf("notification lost or claimed successful: deferred=%v success=%v err=%v", deferred, success, err)
	}
	ids, err := svc.claimTinyStockChecks(t.Context(), row.StoreID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("recovery repeatedly claimed blocked product: %v %v", ids, err)
	}
	// Represent a separately confirmed reconciliation; the stock balance must
	// come from a fresh provider read, not from the old deferred notification.
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET synced_revision=revision WHERE cart_id=$1`, cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET metadata=metadata-'stockRecoveryClaimedAt' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	ids, err = svc.claimTinyStockChecks(t.Context(), row.StoreID)
	if err != nil || len(ids) != 1 || ids[0] != externalID {
		t.Fatalf("resolved product not recovered: %v %v", ids, err)
	}
	if applied, err := svc.refreshERPAvailableStock(t.Context(), row, externalID); err != nil || !applied {
		t.Fatalf("fresh recovery: %v %v", applied, err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT deferred_at IS NOT NULL,last_success_at IS NOT NULL
		FROM erp_stock_sync_state WHERE product_id=$1`, productID).Scan(&deferred, &success); err != nil || deferred || !success || reads != 1 || estoqueDoProduto(t, productID) != 3 {
		t.Fatalf("recovery not completed: deferred=%v success=%v reads=%d stock=%d err=%v", deferred, success, reads, estoqueDoProduto(t, productID), err)
	}
}

func TestStockPendingEditCannotBlockAnotherStore(t *testing.T) {
	svc, row, productID, externalID := stockConsistencyFixture(t, syncedProduct)
	seedStockPendingEdit(t, row.StoreID, productID)
	other, otherRow, otherProductID, _ := stockConsistencyFixture(t, syncedProduct)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET external_id=$2 WHERE id=$1`, otherProductID, externalID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.repo.ApplyERPStockMirror(t.Context(), productID, 20, seqDoProduto(t, productID)); !errors.Is(err, errERPStockPendingEdit) {
		t.Fatalf("pending edit not distinguished: %v", err)
	}
	if applied, err := other.repo.ApplyERPStockMirror(t.Context(), otherProductID, 4, seqDoProduto(t, otherProductID)); err != nil || !applied {
		t.Fatalf("store %s blocked by store %s: %v", otherRow.StoreID, row.StoreID, err)
	}
	if estoqueDoProduto(t, productID) != 10 || estoqueDoProduto(t, otherProductID) != 4 {
		t.Fatal("stock crossed store boundary")
	}
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration/providers"
)

func TestBlockedEditStockRecoveryPreservesUnreconciledUnits(t *testing.T) {
	for _, tc := range []struct {
		name               string
		quantity, retained int
		status, payment    string
	}{
		{name: "removed line", retained: 3, status: "checkout", payment: "pending"},
		{name: "decreased line", quantity: 1, retained: 2, status: "checkout", payment: "pending"},
		{name: "increased line", quantity: 4, status: "checkout", payment: "pending"},
		{name: "payment review", quantity: 1, retained: 2, status: "checkout", payment: "paid"},
		{name: "unconfirmed cancellation", quantity: 1, retained: 2, status: "cancelled", payment: "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, row, product, external := stockConsistencyFixture(t, syncedProduct)
			cart := seedStockPendingEdit(t, row.StoreID, product)
			if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET blocked_at=now(),
				last_error='requires reconciliation' WHERE cart_id=$1`, cart); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status=$2,payment_status=$3 WHERE id=$1`,
				cart, tc.status, tc.payment); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edit_requests SET retained_quantity=$2 WHERE cart_id=$1`,
				cart, tc.retained); err != nil {
				t.Fatal(err)
			}
			if tc.quantity == 0 {
				if _, err := testPool.Exec(t.Context(), `DELETE FROM cart_items WHERE cart_id=$1`, cart); err != nil {
					t.Fatal(err)
				}
			} else if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET quantity=$2,
				erp_confirmed_quantity=1,erp_pending_since=now() WHERE cart_id=$1`, cart, tc.quantity); err != nil {
				t.Fatal(err)
			}
			available := 8
			reads := 0
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(),
				TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
					return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
						reads++
						return available, nil
					}}, nil
				},
			})
			ids, err := svc.claimERPStockChecks(t.Context(), row.StoreID)
			if err != nil || len(ids) != 1 || ids[0] != external {
				t.Fatalf("blocked edit froze recovery: ids=%v err=%v", ids, err)
			}
			for _, balance := range []int{8, 1, 15} {
				available = balance
				want := max(0, balance-tc.quantity-tc.retained)
				applied, err := svc.refreshERPAvailableStock(t.Context(), row, external)
				if err != nil || !applied || estoqueDoProduto(t, product) != want {
					t.Fatalf("ERP=%d applied=%v err=%v stock=%d want=%d",
						balance, applied, err, estoqueDoProduto(t, product), want)
				}
			}
			status, err := cartedit.Read(t.Context(), testPool, cart)
			if err != nil || !status.Pending || !status.Blocked || status.LastError != "requires reconciliation" || reads != 3 {
				t.Fatalf("stock refresh changed journal: %+v reads=%d err=%v", status, reads, err)
			}
		})
	}
}

func TestBlockedEditStockReadStillRejectsWorkerAndConcurrentReservation(t *testing.T) {
	for _, change := range []string{"worker starts", "reservation arrives"} {
		t.Run(change, func(t *testing.T) {
			svc, row, product, external := stockConsistencyFixture(t, syncedProduct)
			cart := seedStockPendingEdit(t, row.StoreID, product)
			if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET blocked_at=now() WHERE cart_id=$1`, cart); err != nil {
				t.Fatal(err)
			}
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(),
				TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
					return tinyStockReaderStub{read: func(ctx context.Context, _ string) (int, error) {
						if change == "worker starts" {
							_, err := testPool.Exec(ctx, `UPDATE cart_erp_edits SET lease_owner=gen_random_uuid(),
								lease_until=now()+interval '3 minutes' WHERE cart_id=$1`, cart)
							return 20, err
						}
						return 20, testRepo.DecrementProductStock(ctx, product, 1)
					}}, nil
				},
			})
			applied, err := svc.refreshERPAvailableStock(t.Context(), row, external)
			wantStock, wantErr := 9, errERPStockSnapshotInvalidated
			if change == "worker starts" {
				wantStock, wantErr = 10, errERPStockPendingEdit
			}
			if applied || !errors.Is(err, wantErr) || estoqueDoProduto(t, product) != wantStock {
				t.Fatalf("stale read accepted: applied=%v err=%v stock=%d", applied, err, estoqueDoProduto(t, product))
			}
		})
	}
}

func TestDeferredStockCreditSurvivesStaleMirrorUntilFreshRead(t *testing.T) {
	svc, _, product, _ := stockConsistencyFixture(t, syncedProduct)
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=0 WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO erp_stock_sync_state
		(product_id,deferred_at,credit_requires_refresh) VALUES($1,now(),true)`, product); err != nil {
		t.Fatal(err)
	}
	seen := seqDoProduto(t, product)
	if err := svc.repo.IncrementProductStock(t.Context(), product, 2); err != nil {
		t.Fatal(err)
	}
	if applied, err := svc.repo.ApplyERPStockMirror(t.Context(), product, 1, seen); err != nil || applied {
		t.Fatalf("stale mirror applied: %v %v", applied, err)
	}
	var deferred bool
	if err := testPool.QueryRow(t.Context(), `SELECT credit_requires_refresh FROM erp_stock_sync_state
		WHERE product_id=$1`, product).Scan(&deferred); err != nil || !deferred || estoqueDoProduto(t, product) != 0 {
		t.Fatalf("stale mirror released credit: deferred=%v stock=%d err=%v", deferred, estoqueDoProduto(t, product), err)
	}
	if applied, err := svc.repo.ApplyERPStockMirror(t.Context(), product, 1, seqDoProduto(t, product)); err != nil || !applied {
		t.Fatalf("fresh mirror rejected: %v %v", applied, err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT credit_requires_refresh FROM erp_stock_sync_state
		WHERE product_id=$1`, product).Scan(&deferred); err != nil || deferred || estoqueDoProduto(t, product) != 1 {
		t.Fatalf("fresh mirror did not recover credit: deferred=%v stock=%d err=%v", deferred, estoqueDoProduto(t, product), err)
	}
	// Once the fresh balance is installed, new reservations can be released normally.
	if err := svc.repo.DecrementProductStock(t.Context(), product, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.repo.IncrementProductStock(t.Context(), product, 1); err != nil {
		t.Fatal(err)
	}
	if estoqueDoProduto(t, product) != 1 {
		t.Fatal("fresh stock remained locked against normal reservation release")
	}
}

func TestDeferredStockCreditRecoversDespiteLateCheckpointAcknowledgement(t *testing.T) {
	svc, row, product, external := stockConsistencyFixture(t, syncedProduct)
	if _, err := testPool.Exec(t.Context(), `INSERT INTO erp_stock_sync_state
		(product_id,last_attempt_at,last_success_at,requested_revision,completed_revision,credit_requires_refresh)
		VALUES($1,'epoch',now(),1,1,true)`, product); err != nil {
		t.Fatal(err)
	}
	reads := 0
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(),
		TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
			return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
				reads++
				return 4, nil
			}}, nil
		},
	})
	ids, err := svc.claimERPStockChecks(t.Context(), row.StoreID)
	if err != nil || len(ids) != 1 || ids[0] != external {
		t.Fatalf("late checkpoint hid pending credit: ids=%v err=%v", ids, err)
	}
	if applied, err := svc.refreshERPAvailableStockRevision(t.Context(), row, external, 1); err != nil || !applied {
		t.Fatalf("fresh stock recovery: applied=%v err=%v", applied, err)
	}
	if reads != 1 || estoqueDoProduto(t, product) != 4 {
		t.Fatalf("completed revision skipped required read: reads=%d stock=%d", reads, estoqueDoProduto(t, product))
	}
}

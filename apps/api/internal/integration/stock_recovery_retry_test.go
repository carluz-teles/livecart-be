//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

func TestStockRecoveryFailurePersistsRetryWithoutWebhook(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "provider failure"
		if cancelled {
			name = "deadline cancellation"
		}
		t.Run(name, func(t *testing.T) {
			svc, row, product, ext := stockConsistencyFixture(t, syncedProduct)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads := 0
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
				return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
					reads++
					if reads == 1 {
						if cancelled {
							cancel()
							return 0, ctx.Err()
						}
						return 0, errors.New("provider unavailable")
					}
					return 4, nil
				}}, nil
			}})
			if applied, err := svc.refreshERPAvailableStock(ctx, row, ext); err == nil || applied {
				t.Fatalf("failed read applied: %v %v", applied, err)
			}
			var retained bool
			if err := testPool.QueryRow(t.Context(), `SELECT deferred_at IS NOT NULL AND read_owner IS NULL AND read_until IS NULL AND requested_revision=completed_revision FROM erp_stock_sync_state WHERE product_id=$1`, product).Scan(&retained); err != nil || !retained {
				t.Fatalf("periodic failure lost its retry: %v %v", retained, err)
			}
			if estoqueDoProduto(t, product) != 10 {
				t.Fatal("failed read changed available stock")
			}
			if applied, err := svc.refreshERPAvailableStock(t.Context(), row, ext); err != nil || !applied {
				t.Fatalf("retry: %v %v", applied, err)
			}
			if err := testPool.QueryRow(t.Context(), `SELECT deferred_at IS NULL AND last_success_at IS NOT NULL AND read_owner IS NULL FROM erp_stock_sync_state WHERE product_id=$1`, product).Scan(&retained); err != nil || !retained || estoqueDoProduto(t, product) != 4 {
				t.Fatalf("successful retry did not complete checkpoint: %v %v", retained, err)
			}
		})
	}
}

func TestStockRecoveryExpiredReaderCannotDirtyNewOwner(t *testing.T) {
	svc, row, product, ext := stockConsistencyFixture(t, syncedProduct)
	nextOwner := uuid.NewString()
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
			if _, err := testPool.Exec(t.Context(), `UPDATE erp_stock_sync_state SET read_owner=$2,read_until=now()+interval '2 minutes',last_success_at=now(),deferred_at=NULL WHERE product_id=$1`, product, nextOwner); err != nil {
				t.Fatal(err)
			}
			return 0, errors.New("expired reader failed")
		}}, nil
	}})
	if _, err := svc.refreshERPAvailableStock(t.Context(), row, ext); err == nil {
		t.Fatal("failure hidden")
	}
	var preserved bool
	if err := testPool.QueryRow(t.Context(), `SELECT read_owner=$2 AND deferred_at IS NULL AND last_success_at IS NOT NULL FROM erp_stock_sync_state WHERE product_id=$1`, product, nextOwner).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("stale reader changed new owner's checkpoint: %v %v", preserved, err)
	}
}

func TestStockRecoveryReservesRetriesAndRetainsUnattemptedClaims(t *testing.T) {
	svc, row, retryProduct, retryExt := stockConsistencyFixture(t, syncedProduct)
	if _, err := testPool.Exec(t.Context(), `INSERT INTO erp_stock_sync_state(product_id,last_attempt_at,last_success_at,deferred_at) VALUES($1,now()-interval '6 minutes',now()-interval '1 hour',now()-interval '6 minutes')`, retryProduct); err != nil {
		t.Fatal(err)
	}
	var event string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO live_events(store_id,title,status,ends_at) VALUES($1,'retry priority','active',now()+interval '1 day') RETURNING id::text`, row.StoreID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	for n := range 12 {
		var id string
		if err := testPool.QueryRow(t.Context(), `INSERT INTO products(store_id,name,keyword,external_source,external_id,price,stock) VALUES($1,'waiting',($2+100)::text,'tiny','waiting-'||$2::text,1000,0) RETURNING id::text`, row.StoreID, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		seedQueueWaiter(t, scaleFixture{storeID: row.StoreID, eventID: event}, id, 1)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO products(store_id,name,keyword,external_source,external_id,price,stock) SELECT $1,'catalogue',($2+n)::text,'tiny','catalogue-'||n,1000,0 FROM generate_series(1,3) n`, row.StoreID, 200); err != nil {
		t.Fatal(err)
	}
	ids, err := svc.claimERPStockChecks(t.Context(), row.StoreID)
	if err != nil || len(ids) != 10 || !slices.Contains(ids, retryExt) {
		t.Fatalf("retry starved: %v %v", ids, err)
	}
	if ids[1] != retryExt {
		t.Fatalf("retry will be delayed past the account budget: %v", ids)
	}
	catalogue := 0
	for _, ext := range ids {
		if slices.Contains([]string{"catalogue-1", "catalogue-2", "catalogue-3"}, ext) {
			catalogue++
		}
	}
	if catalogue != 2 {
		t.Fatalf("catalogue starved: %v", ids)
	}
	var pending int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM erp_stock_sync_state s JOIN products p ON p.id=s.product_id WHERE p.store_id=$1 AND p.external_id=ANY($2) AND s.deferred_at IS NOT NULL AND s.read_owner IS NULL`, row.StoreID, ids).Scan(&pending); err != nil || pending != 10 {
		t.Fatalf("unattempted claims lost recovery: pending=%d err=%v", pending, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET metadata=metadata||jsonb_build_object('stockRecoveryClaimedAt',now()-interval '2 minutes') WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	next, err := svc.claimERPStockChecks(t.Context(), row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range next {
		if slices.Contains(ids, id) {
			t.Fatalf("retry ignored five-minute backoff: %v", next)
		}
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE erp_stock_sync_state SET last_attempt_at=now()-interval '6 minutes' WHERE product_id=$1`, retryProduct); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET metadata=metadata||jsonb_build_object('stockRecoveryClaimedAt',now()-interval '2 minutes') WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	next, err = svc.claimERPStockChecks(t.Context(), row.StoreID)
	if err != nil || !slices.Contains(next, retryExt) {
		t.Fatalf("retry lost after backoff: %v %v", next, err)
	}
}

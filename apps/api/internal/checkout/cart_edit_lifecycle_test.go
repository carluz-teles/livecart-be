//go:build integration

package checkout

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/inventory"
	"livecart/apps/api/internal/live"
)

func assertEditStock(t *testing.T, f editFixture, want int) {
	t.Helper()
	var stock int
	if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != want {
		t.Fatalf("stock=%d want=%d", stock, want)
	}
}

func TestCartEdit_ManualProductInRemotePurchaseClearsPendingMarker(t *testing.T) {
	f := seedMerchantEdit(t)
	f.service.integrationService = integration.NewService(integration.NewRepository(testQueries, testPool), nil, nil, nil, nil, zap.NewNop())
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET external_id=NULL,external_source='manual' WHERE id=$1`, f.product); err != nil {
		t.Fatal(err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { return nil }}
	result, err := f.service.UpdateCartItemQuantity(t.Context(), MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: 1})
	if err != nil || result.ERPItemSync.Pending {
		t.Fatalf("manual line remained pending: %+v %v", result, err)
	}
	var pending bool
	var confirmed int
	if err := testPool.QueryRow(t.Context(), `SELECT erp_pending_since IS NOT NULL,erp_confirmed_quantity FROM cart_items WHERE id=$1`, f.item).Scan(&pending, &confirmed); err != nil {
		t.Fatal(err)
	}
	if pending || confirmed != 1 {
		t.Fatalf("manual item marker=%v confirmed=%d", pending, confirmed)
	}
	assertEditStock(t, f, 9)
}

func TestCartEdit_DetachedOriginCannotReleaseRetention(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(fmt.Sprint("during_erp_", during), func(t *testing.T) {
			f := seedMerchantEdit(t)
			child, token, item := seedEditChild(t, f)
			if err := f.service.RemoveCartItemAsMerchant(t.Context(), token, item); err != nil {
				t.Fatal(err)
			}
			detach := func() error {
				_, err := testPool.Exec(t.Context(), `UPDATE carts SET joined_to_cart_id=NULL WHERE id=$1`, child)
				return err
			}
			calls := 0
			f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error {
				calls++
				return detach()
			}}
			if !during {
				if err := detach(); err != nil {
					t.Fatal(err)
				}
			}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			status, err := cartedit.Read(t.Context(), testPool, f.cart)
			if err != nil || !status.Pending || !status.Blocked || (!during && calls != 0) {
				t.Fatalf("detached journal acknowledged: %+v calls=%d err=%v", status, calls, err)
			}
			assertEditStock(t, f, 8)
		})
	}
}

func TestCartEdit_ReplayKeepsIdentityAfterJoiningPurchase(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy_origin_", legacy), func(t *testing.T) {
			f := seedMerchantEdit(t)
			ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
			if err := f.service.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
				t.Fatal(err)
			}
			f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { return nil }}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			if legacy {
				if _, err := testPool.Exec(ctx, `UPDATE cart_erp_edit_requests SET request=request-'originCartId' WHERE cart_id=$1`, f.cart); err != nil {
					t.Fatal(err)
				}
			}
			host, _, _ := seedEditChild(t, f)
			if _, err := testPool.Exec(ctx, `UPDATE carts SET joined_to_cart_id=NULL WHERE id=$1`, host); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `UPDATE carts SET joined_to_cart_id=$2 WHERE id=$1`, f.cart, host); err != nil {
				t.Fatal(err)
			}
			if err := f.service.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
				t.Fatalf("replay failed after join: %v", err)
			}
			assertEditStock(t, f, 10)
		})
	}
}

func TestCartEdit_CancellationNeedsRemoteEvidence(t *testing.T) {
	f := seedMerchantEdit(t)
	if err := f.service.RemoveCartItemAsMerchant(t.Context(), f.token, f.item); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status='cancelled',erp_order_state='cancelled' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{verify: func(context.Context, string, string) error { return erp.ErrCancellationUnconfirmed }}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 8)
	status, err := cartedit.Read(t.Context(), testPool, f.cart)
	if err != nil || !status.Blocked || !status.Pending {
		t.Fatalf("missing reconciliation: %+v %v", status, err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 8) // A blocked ERP edit now waits for a fresh stock read before releasing retention.
}

func TestCartEdit_JoinLockRejectsNewCommandButAllowsReplay(t *testing.T) {
	f := seedMerchantEdit(t)
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	if err := f.service.SetCartItemQuantityAsMerchant(ctx, f.token, f.item, 1); err != nil {
		t.Fatal(err)
	}
	conn, err := testPool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), `SELECT pg_advisory_lock(hashtextextended($1,0))`, "cart_topology:"+f.cart); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "cart_topology:"+f.cart)
	if err := f.service.SetCartItemQuantityAsMerchant(ctx, f.token, f.item, 1); err != nil {
		t.Fatalf("replay rejected: %v", err)
	}
	if err := f.service.SetCartItemQuantityAsMerchant(t.Context(), f.token, f.item, 3); err == nil {
		t.Fatal("accepted during join")
	}
	assertEditStock(t, f, 8)
}

func TestCartEdit_PromotedRemovalWaitsForERPAndSurvivesRestart(t *testing.T) {
	f := seedMerchantEdit(t)
	// Exercise a real allocation, whose completed waitlist history is notified.
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET waitlisted_quantity=2 WHERE id=$1`, f.item); err != nil {
		t.Fatal(err)
	}
	request := requestPriceLot(t, priceLotFixture{cart: f.cart, product: f.product}, 2, 1000)
	repo := integration.NewRepository(testQueries, testPool)
	promoted, err := repo.PromoteNextWaitlistEntry(t.Context(), f.store, f.product)
	if err != nil || promoted == nil || promoted.Quantity != 2 {
		t.Fatalf("promotion=%+v err=%v", promoted, err)
	}
	ctx := cartedit.WithRequestID(t.Context(), uuid.NewString())
	if err := f.service.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 6)
	if changed, err := repo.CancelWaitingRequest(t.Context(), request, f.cart); err != nil || changed {
		t.Fatalf("notified history released stock: %v %v", changed, err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { return errors.New("ERP timeout") }}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 6)
	fresh := NewService(testRepo, testPool, nil, nil, zap.NewNop())
	fresh.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { return nil }}
	dueMerchantEdit(t, f.cart)
	fresh.RecoverMerchantEdits(t.Context())
	fresh.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 8)
	if err := fresh.RemoveCartItemAsMerchant(ctx, f.token, f.item); err != nil {
		t.Fatalf("removal retry after deletion: %v", err)
	}
	assertEditStock(t, f, 8)
}

func TestCartEdit_CancellationReleasesCurrentItemsAndRetentionOnce(t *testing.T) {
	for _, quantity := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("quantity_%d", quantity), func(t *testing.T) {
			f := seedMerchantEdit(t)
			operation := "set"
			if quantity == 0 {
				operation = "remove"
			}
			if _, err := f.service.queueMerchantEdit(t.Context(), MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: quantity, ByMerchant: true}, operation); err != nil {
				t.Fatal(err)
			}
			repo := integration.NewRepository(testQueries, testPool)
			if result, err := repo.CancelCartAndReleaseStock(t.Context(), f.cart, f.store); err != nil || !result.Eligible {
				t.Fatalf("cancel=%+v %v", result, err)
			}
			// The old ERP order is still reserved: retain any removed units.
			f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { t.Error("mutated cancelled purchase"); return nil }}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			assertEditStock(t, f, 10-max(2-quantity, 0))
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_state='cancelled' WHERE id=$1`, f.cart); err != nil {
				t.Fatal(err)
			}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			f.service.RecoverMerchantEdits(t.Context())
			assertEditStock(t, f, 10)
		})
	}
}

func seedEditChild(t *testing.T, f editFixture) (string, string, string) {
	t.Helper()
	var cart, token, item string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,payment_status,joined_to_cart_id)
	 SELECT event_id,store_id,platform_user_id||'-child',platform_handle||'_child',token||'-child',short_id+100000,'checkout','unpaid',id FROM carts WHERE id=$1 RETURNING id::text,token`, f.cart).Scan(&cart, &token); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price,erp_confirmed_quantity) VALUES($1,$2,2,1000,2) RETURNING id::text`, cart, f.product).Scan(&item); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM carts WHERE id=$1`, cart) })
	return cart, token, item
}

func TestCartEdit_JoinedChildCancellationDuringERPRequiresNewGrid(t *testing.T) {
	f := seedMerchantEdit(t)
	child, token, item := seedEditChild(t, f)
	if _, err := f.service.queueMerchantEdit(t.Context(), MutateCartItemInput{Token: token, ItemID: item, Quantity: 1, ByMerchant: true}, "set"); err != nil {
		t.Fatal(err)
	}
	if err := cartedit.AssertReady(t.Context(), testPool, child); err == nil {
		t.Fatal("child bypassed canonical pending state")
	}
	repo := integration.NewRepository(testQueries, testPool)
	first := true
	f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(ctx context.Context, cart, _ string) error {
		if cart != f.cart {
			return errors.New("did not synchronize canonical host")
		}
		grid, err := repo.ListCartGridItems(ctx, cart)
		if err != nil {
			return err
		}
		if first {
			first = false
			if len(grid) != 1 || grid[0].Quantity != 3 {
				return fmt.Errorf("first grid=%+v", grid)
			}
			_, err := repo.CancelCartAndReleaseStock(ctx, child, f.store)
			return err
		}
		if len(grid) != 1 || grid[0].Quantity != 2 {
			return fmt.Errorf("cancelled child remained in ERP grid=%+v", grid)
		}
		return nil
	}}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	status, err := cartedit.Read(t.Context(), testPool, f.cart)
	if err != nil || !status.Pending {
		t.Fatalf("accepted obsolete group grid: %+v %v", status, err)
	}
	assertEditStock(t, f, 9) // Cancellation credits the child's remaining unit only.
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	if err := cartedit.AssertReady(t.Context(), testPool, f.cart); err != nil {
		t.Fatal(err)
	}
	assertEditStock(t, f, 10)
}

func TestCartEdit_LocalAndNativeCreationFollowAcceptedMode(t *testing.T) {
	for _, mode := range []string{"manual", "bling-local", "tiny-native"} {
		t.Run(mode, func(t *testing.T) {
			f := seedMerchantEdit(t)
			f.service.integrationService = integration.NewService(integration.NewRepository(testQueries, testPool), nil, nil, nil, nil, zap.NewNop())
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_state='none',external_order_id=NULL WHERE id=$1`, f.cart); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "manual":
				if _, err := testPool.Exec(t.Context(), `DELETE FROM integrations WHERE store_id=$1`, f.store); err != nil {
					t.Fatal(err)
				}
			case "bling-local":
				if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET provider='bling' WHERE store_id=$1`, f.store); err != nil {
					t.Fatal(err)
				}
			}
			created, written := 0, 0
			f.service.merchantEditERP = &scriptedMerchantERP{
				ensure: func(context.Context, string, string) error { created++; return nil },
				mutate: func(context.Context, string, string) error { written++; return nil },
			}
			result, err := f.service.UpdateCartItemQuantity(t.Context(), MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: 1})
			if err != nil || result.ERPItemSync.Pending {
				t.Fatalf("local/creation result=%+v %v", result, err)
			}
			assertEditStock(t, f, 9)
			want := 0
			if mode == "tiny-native" {
				want = 1
			}
			if created != want || written != want {
				t.Fatalf("create=%d write=%d want=%d", created, written, want)
			}
		})
	}
}

func TestCartEdit_DisconnectedIntegrationCannotReleaseOrBecomeLocal(t *testing.T) {
	f := seedMerchantEdit(t)
	if err := f.service.RemoveCartItemAsMerchant(t.Context(), f.token, f.item); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET status='disconnected' WHERE store_id=$1`, f.store); err != nil {
		t.Fatal(err)
	}
	f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { t.Error("wrote to disconnected integration"); return nil }}
	dueMerchantEdit(t, f.cart)
	f.service.RecoverMerchantEdits(t.Context())
	assertEditStock(t, f, 8)
	if status, err := cartedit.Read(t.Context(), testPool, f.cart); err != nil || !status.Pending {
		t.Fatalf("lost obligation: %+v %v", status, err)
	}
}

func TestCartEdit_PendingPurchaseDefersLiveAndWaitlist(t *testing.T) {
	f := seedMerchantEdit(t)
	var event, user, handle string
	if err := testPool.QueryRow(t.Context(), `SELECT event_id::text,platform_user_id,platform_handle FROM carts WHERE id=$1`, f.cart).Scan(&event, &user, &handle); err != nil {
		t.Fatal(err)
	}
	if err := f.service.SetCartItemQuantityAsMerchant(t.Context(), f.token, f.item, 1); err != nil {
		t.Fatal(err)
	}
	comments := live.NewService(live.NewRepository(testQueries, testPool), zap.NewNop())
	_, err := comments.ApplyCommentItem(t.Context(), live.AddToCartInput{StoreID: f.store, EventID: event, ProductID: f.product, ProductPrice: 1000, PlatformUserID: user, PlatformHandle: handle, Quantity: 1}, uuid.NewString())
	assertEditPendingConflict(t, err)
	// A waiting request on another product in this purchase must also defer.
	var other string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO products(store_id,name,keyword,price,stock,external_source) VALUES($1,'Waiting','9998',1000,1,'manual') RETURNING id::text`, f.store).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_items(cart_id,product_id,quantity,waitlisted_quantity,unit_price) VALUES($1,$2,1,1,1000)`, f.cart, other); err != nil {
		t.Fatal(err)
	}
	requestPriceLot(t, priceLotFixture{cart: f.cart, product: other}, 1, 1000)
	repo := integration.NewRepository(testQueries, testPool)
	if result, err := repo.PromoteNextWaitlistEntry(t.Context(), f.store, other); result != nil || !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) {
		t.Fatalf("promotion=%+v err=%v", result, err)
	}
	assertEditStock(t, f, 8)
}

func TestCartEdit_PublicHTTPPreservesIdempotencyKey(t *testing.T) {
	f := seedMerchantEdit(t)
	f.service.integrationService = integration.NewService(integration.NewRepository(testQueries, testPool), nil, nil, nil, nil, zap.NewNop())
	f.service.merchantEditERP = &scriptedMerchantERP{mutate: func(context.Context, string, string) error { return errors.New("offline") }}
	app := fiber.New()
	NewHandler(f.service, nil).RegisterRoutes(app)
	key := uuid.NewString()
	for range 2 {
		req := httptest.NewRequest("POST", "/api/public/checkout/"+f.token+"/items", strings.NewReader(fmt.Sprintf(`{"productId":%q,"quantity":1}`, f.product)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("HTTP status=%d", res.StatusCode)
		}
	}
	assertEditStock(t, f, 7)
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM cart_erp_edit_requests WHERE cart_id=$1`, f.cart).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("accepted repeated addition %d times", count)
	}
}

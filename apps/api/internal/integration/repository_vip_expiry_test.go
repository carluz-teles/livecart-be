//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/customer"
)

func TestVIPExpiryUsesActiveMembership(t *testing.T) {
	cases := []struct {
		name    string
		member  bool
		removed bool
		expires bool
	}{
		{name: "active_vip_with_legacy_deadline", member: true},
		{name: "ordinary_buyer", expires: true},
		{name: "removed_vip_with_ordinary_cart", member: true, removed: true, expires: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireDB(t)
			fx := seedVipBuyer(t)
			cart := seedOpenCart(t, fx, fx.eventA, fx.productA, 3, time.Hour, "checkout")
			setCartExpiresAt(t, cart, time.Now().Add(-time.Minute))
			if tc.member {
				_, err := testPool.Exec(t.Context(), `INSERT INTO vip_handles(store_id,platform_handle,removed_at)
                    VALUES($1,$2,CASE WHEN $3::boolean THEN now() ELSE NULL END)`, fx.storeID, fx.handle, tc.removed)
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := testRepo.ExpireCartAndReleaseStock(t.Context(), cart, fx.storeID)
			if err != nil {
				t.Fatal(err)
			}
			var stock, facts int
			if err := testPool.QueryRow(t.Context(), `SELECT stock FROM products WHERE id=$1`, fx.productA).Scan(&stock); err != nil {
				t.Fatal(err)
			}
			if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox
                    WHERE name='cart.expired' AND payload->>'cart_id'=$1`, cart).Scan(&facts); err != nil {
				t.Fatal(err)
			}
			wantStock, wantFacts, wantStatus := 10, 0, "checkout"
			if tc.expires {
				wantStock, wantFacts, wantStatus = 13, 1, "expired"
			}
			if result.Eligible != tc.expires || stock != wantStock || facts != wantFacts || cartStatus(t, cart) != wantStatus {
				t.Fatalf("eligible=%v stock=%d expiry_events=%d status=%s", result.Eligible, stock, facts, cartStatus(t, cart))
			}
		})
	}
}

type realVIPActivator struct{ svc *Service }

func (a realVIPActivator) ActivateVipCartsForHandle(ctx context.Context, store, handle string) (customer.VipActivation, error) {
	result, err := a.svc.ActivateVipCartsForHandle(ctx, store, handle)
	return customer.VipActivation(result), err
}

func TestVIPConsolidationWaitsForPendingEditInEveryCandidate(t *testing.T) {
	for _, destination := range []bool{false, true} {
		name := "origin_without_erp_order"
		if destination {
			name = "destination"
		}
		t.Run(name, func(t *testing.T) {
			requireDB(t)
			fx := seedVipBuyer(t)
			source := seedOpenCart(t, fx, fx.eventA, fx.productA, 2, time.Hour, "checkout")
			dest := seedOpenCart(t, fx, fx.eventB, fx.productB, 1, 0, "checkout")
			pending, product := source, fx.productA
			if destination {
				pending, product = dest, fx.productB
			}
			if _, err := testPool.Exec(t.Context(), `INSERT INTO integrations(store_id,type,provider,status) VALUES($1,'erp','tiny','active')`, fx.storeID); err != nil {
				t.Fatal(err)
			}
			seedRecoveryEdit(t, finFixture{cartID: pending, storeID: fx.storeID, productID: product})
			svc := customer.NewService(customer.NewRepository(sqlc.New(testPool)), zap.NewNop())
			svc.SetVipCartActivator(realVIPActivator{svc: newFinalisationService(newScriptedERP())})
			input := customer.AddVipInput{StoreID: uuid.MustParse(fx.storeID), Handle: "@" + fx.handle}
			result, err := svc.AddVipHandle(t.Context(), input)
			if err != nil || !result.ActivationFailed() {
				t.Fatalf("VIP should persist with deferred consolidation: %+v %v", result, err)
			}
			for _, id := range []string{source, dest} {
				status, eternal, deadline, _ := cartState(t, id)
				if status != "checkout" || eternal || deadline {
					t.Fatalf("cart moved or lost VIP protection: %s %v %v", status, eternal, deadline)
				}
			}
			var quantity int
			if err := testPool.QueryRow(t.Context(), `SELECT quantity FROM cart_items WHERE cart_id=$1 AND product_id=$2`, source, fx.productA).Scan(&quantity); err != nil {
				t.Fatal(err)
			}
			if quantity != 2 {
				t.Fatalf("origin items moved: %d", quantity)
			}
			status, err := cartedit.Read(t.Context(), testPool, pending)
			if err != nil || !status.Pending {
				t.Fatalf("journal changed: %+v %v", status, err)
			}
			// This test covers the consolidation contract at the durable ACK
			// boundary; checkout tests execute the actual ERP worker producing it.
			if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET synced_revision=revision WHERE cart_id=$1`, pending); err != nil {
				t.Fatal(err)
			}
			result, err = svc.AddVipHandle(t.Context(), input)
			if err != nil || result.ActivationFailed() {
				t.Fatalf("retry after acknowledgement failed: %+v %v", result, err)
			}
			if cartStatus(t, source) != "cancelled" {
				t.Fatal("acknowledged origin was not consolidated")
			}
			_, eternal, deadline, _ := cartState(t, dest)
			if !eternal || deadline {
				t.Fatal("destination did not become eternal")
			}
		})
	}
}

type failingVIPActivator struct{}

func (failingVIPActivator) ActivateVipCartsForHandle(context.Context, string, string) (customer.VipActivation, error) {
	return customer.VipActivation{}, errors.New("activation unavailable")
}

func TestVIPPromotionProtectsAllCartsBeforeConsolidation(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	carts := []string{
		seedOpenCart(t, fx, fx.eventA, fx.productA, 1, time.Hour, "checkout"),
		seedOpenCart(t, fx, fx.eventB, fx.productB, 1, 0, "active"),
	}
	svc := customer.NewService(customer.NewRepository(sqlc.New(testPool)), zap.NewNop())
	svc.SetVipCartActivator(failingVIPActivator{})
	result, err := svc.AddVipHandle(t.Context(), customer.AddVipInput{
		StoreID: uuid.MustParse(fx.storeID), Handle: "@" + fx.handle,
	})
	if err != nil || !result.ActivationFailed() {
		t.Fatalf("expected persisted membership with consolidation failure: %v %v", result, err)
	}
	for _, cart := range carts {
		_, eternal, deadline, _ := cartState(t, cart)
		if eternal || deadline {
			t.Fatalf("cart %s: eternal=%v deadline=%v; protection must survive failed consolidation", cart, eternal, deadline)
		}
	}
}

func TestVIPExpiryDeliveryRechecksCart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		vip    bool
	}{
		{name: "vip_became_active_before_delivery", status: "expired", vip: true},
		{name: "reopened_cart", status: "checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireDB(t)
			fx := seedPaidCart(t, 1, 0)
			fake := newScriptedERP()
			svc := newFinalisationService(fake)
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='pending',paid_amount_cents=0,
                    purchase_closed=false,status=$2,expires_at=now()-interval '1 minute' WHERE id=$1`, fx.cartID, tc.status); err != nil {
				t.Fatal(err)
			}
			if err := svc.EnsureERPOrderForCart(t.Context(), fx.cartID, fx.storeID); err != nil {
				t.Fatal(err)
			}
			if tc.vip {
				if _, err := testPool.Exec(t.Context(), `INSERT INTO vip_handles(store_id,platform_handle)
						SELECT $2,lower(ltrim(platform_handle,'@')) FROM carts WHERE id=$1`, fx.cartID, fx.storeID); err != nil {
					t.Fatal(err)
				}
			}
			before := len(fake.calls)
			if err := svc.ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID); err != nil {
				t.Fatal(err)
			}
			if len(fake.calls) != before {
				t.Fatalf("stale expiry reached ERP: %v", fake.calls[before:])
			}
		})
	}
}

func TestVIPManualCancellationStillReleasesERPOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='pending',paid_amount_cents=0,
        purchase_closed=false,status='checkout',never_expires=true,expires_at=NULL WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureERPOrderForCart(t.Context(), fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO vip_handles(store_id,platform_handle)
        SELECT $2,lower(ltrim(platform_handle,'@')) FROM carts WHERE id=$1`, fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	result, err := testRepo.CancelCartAndReleaseStock(t.Context(), fx.cartID, fx.storeID)
	if err != nil || !result.Eligible {
		t.Fatalf("manual VIP cancellation was refused: %+v %v", result, err)
	}
	for range 2 {
		if err := svc.ReactCartCancelledERP(t.Context(), fx.cartID, fx.storeID); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.callsWithPrefix("Situacao:")) != 1 || fake.count("Reverse:") != 0 {
		t.Fatalf("manual cancel must cancel exactly once without reversing stock: %v", fake.calls)
	}
}

func TestVIPLegacyCartRetainsWaitlistAndCanReceiveStock(t *testing.T) {
	requireDB(t)
	fx := seedWaitlistCloseFixture(t)
	cart := fx.carts["waiting"]
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET store_id=$2 WHERE id=$1`, cart, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO vip_handles(store_id,platform_handle) VALUES($1,'waiting')`, fx.storeID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := testRepo.GetCartExpirySnapshot(t.Context(), cart)
	if err != nil || !snapshot.Protected {
		t.Fatalf("scheduler did not recognize membership: %+v %v", snapshot, err)
	}
	deadline, err := testRepo.GetNextEventWaitlistDeadline(t.Context(), fx.eventID)
	if err != nil || deadline != nil {
		t.Fatalf("VIP armed waitlist expiry: %v %v", deadline, err)
	}
	if _, err := testRepo.ExpireEventWaitlist(t.Context(), fx.eventID); err != nil {
		t.Fatal(err)
	}
	if got := waitlistStatusByCart(t, cart); got != "waiting" {
		t.Fatalf("VIP lost its waitlist: %s", got)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=1 WHERE id=$1`, fx.productID); err != nil {
		t.Fatal(err)
	}
	result, err := testRepo.PromoteNextWaitlistEntry(t.Context(), fx.storeID, fx.productID)
	if err != nil || result == nil || result.CartID != cart {
		t.Fatalf("VIP with stale deadline could not receive stock: %+v %v", result, err)
	}
}

func TestVIPCommercialCloseDoesNotReintroduceDeadline(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	cart := seedOpenCart(t, fx, fx.eventB, fx.productB, 1, 0, "active")
	if _, err := testPool.Exec(t.Context(), `INSERT INTO vip_handles(store_id,platform_handle) VALUES($1,$2)`, fx.storeID, fx.handle); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET expires_at=NULL WHERE id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE live_events SET status='ended' WHERE id=$1`, fx.eventB); err != nil {
		t.Fatal(err)
	}
	eventID, err := parseUUID(fx.eventB)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := testRepo.queries.FinalizeCartsByEvent(t.Context(), sqlc.FinalizeCartsByEventParams{
		EventID: eventID, ExpirationMinutes: 10, WaitlistExtraMinutes: 5,
	})
	if err != nil || len(rows) != 0 {
		t.Fatalf("VIP was armed for expiry on close: %v %v", rows, err)
	}
	_, _, hasDeadline, _ := cartState(t, cart)
	if hasDeadline {
		t.Fatal("commercial-close trigger restored VIP expiry")
	}
}

func TestVIPRecoverySkipsLegacyDeadline(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	cart := seedOpenCart(t, fx, fx.eventA, fx.productA, 1, 0, "checkout")
	setCartExpiresAt(t, cart, time.Now().Add(-time.Hour))
	if _, err := testPool.Exec(t.Context(), `INSERT INTO vip_handles(store_id,platform_handle) VALUES($1,$2)`, fx.storeID, fx.handle); err != nil {
		t.Fatal(err)
	}
	scaleService().RecoverRecentCartExpiries(t.Context())
	if cartStatus(t, cart) != "checkout" {
		t.Fatal("recovery sweep expired active VIP")
	}
}

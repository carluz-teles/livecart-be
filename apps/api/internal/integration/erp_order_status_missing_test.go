//go:build integration

package integration

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
)

func TestMissingERPOrderSweepPreservesOldAndCurrentPurchases(t *testing.T) {
	requireDB(t)
	for _, vip := range []bool{false, true} {
		name := "event"
		if vip {
			name = "vip_across_events"
		}
		t.Run(name, func(t *testing.T) {
			fx := seedPaidCart(t, 2, 0)
			fake := newScriptedERP()
			svc := newFinalisationService(fake)
			ctx := t.Context()
			if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
				t.Fatal(err)
			}
			_, oldOrder, _, _ := cartERPState(t, fx.cartID)
			if err := svc.ERP().ObserveOrderStatus(ctx, fx.storeID, oldOrder, "",
				providers.ERPOrderStatusPreparandoEnvio, erp.StatusSourceWebhook, nil); err != nil {
				t.Fatal(err)
			}
			_, err := testPool.Exec(ctx, `UPDATE carts SET payment_status='pending',paid_at=NULL,
				never_expires=$2,erp_order_status_at=now()-interval '1 year' WHERE id=$1`, fx.cartID, vip)
			if err != nil {
				t.Fatal(err)
			}
			nextEvent := fx.eventID
			if vip {
				err := testPool.QueryRow(ctx, `INSERT INTO live_events(store_id,status,title,ends_at)
					VALUES($1,'active','Next VIP purchase',now()+interval '1 day') RETURNING id::text`, fx.storeID).Scan(&nextEvent)
				if err != nil {
					t.Fatal(err)
				}
			}
			var newCart string
			err = testPool.QueryRow(ctx, `INSERT INTO carts
				(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,payment_status,
				 never_expires,external_order_id,erp_order_status,erp_order_state)
				SELECT $2,store_id,platform_user_id,platform_handle,'new-'||token,short_id+100000,
				 'active','pending',never_expires,'new-'||external_order_id,'aberto','open'
				FROM carts WHERE id=$1 RETURNING id::text`, fx.cartID, nextEvent).Scan(&newCart)
			if err != nil {
				t.Fatal(err)
			}
			// Check all local fields and item rows, not just the two ERP links.
			snapshot := func() []byte {
				t.Helper()
				var data []byte
				err := testPool.QueryRow(ctx, `SELECT jsonb_build_object(
					'carts',(SELECT jsonb_agg(to_jsonb(c)-'erp_order_status'-'erp_order_status_at' ORDER BY id)
					 FROM carts c WHERE store_id=$1),
					'items',(SELECT jsonb_agg(to_jsonb(ci) ORDER BY ci.id) FROM cart_items ci
					 JOIN carts c ON c.id=ci.cart_id WHERE c.store_id=$1))`, fx.storeID).Scan(&data)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			before := snapshot()
			callsBefore := len(fake.callsWithPrefix(""))
			fake.sumidos[oldOrder] = true
			svc.RunERPOrderStatusSweep(ctx, 30*24*time.Hour, 100)
			_, orderAfter, status, _ := cartERPState(t, fx.cartID)
			if orderAfter != oldOrder || status != string(providers.ERPOrderStatusNaoEncontrado) {
				t.Fatalf("missing observation failed or unlinked old order: order=%s status=%s", orderAfter, status)
			}
			history, err := testRepo.ListERPOrderStatusHistory(ctx, fx.cartID)
			if err != nil || len(history) != 3 {
				t.Fatalf("expected create, preparing and missing history: %+v err=%v", history, err)
			}
			latest := history[0]
			if latest.Status != "nao_encontrado" || latest.PreviousStatus != "preparando_envio" ||
				latest.Source != erp.StatusSourceSweep || latest.ExternalOrderID != oldOrder {
				t.Fatalf("incorrect missing transition: %+v", latest)
			}
			// Repeat the observation and sweep after making its timestamp stale
			// again: terminal status, not just recency, must stop further reads.
			if err := svc.ERP().ObserveOrderStatus(ctx, fx.storeID, oldOrder, "",
				providers.ERPOrderStatusNaoEncontrado, erp.StatusSourceSweep, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_order_status_at=now()-interval '1 year'
				WHERE id=$1`, fx.cartID); err != nil {
				t.Fatal(err)
			}
			svc.RunERPOrderStatusSweep(ctx, 30*24*time.Hour, 100)
			if got := len(fake.callsWithPrefix("GetSituacao:" + oldOrder)); got != 1 {
				t.Fatalf("missing order queried %d times, want exactly once", got)
			}
			history, err = testRepo.ListERPOrderStatusHistory(ctx, fx.cartID)
			if err != nil || len(history) != 3 {
				t.Fatalf("redelivery duplicated status history: %+v err=%v", history, err)
			}
			// The same Go boundary protects mutation, joining and expiry. The
			// passive observation cannot turn into automatic order recreation.
			err = svc.ERP().MutateERPOrderItems(ctx, fx.cartID, fx.storeID)
			if !errors.Is(err, erp.ErrPedidoFaturado) {
				t.Fatalf("observed missing purchase accepted item mutation: %v", err)
			}
			join, err := testRepo.GetCartForJoin(ctx, fx.cartID, fx.storeID)
			if err != nil || !join.Invoiced {
				t.Fatalf("missing order is not blocked for joining: %+v err=%v", join, err)
			}
			expiry, err := testRepo.GetCartExpirySnapshot(ctx, fx.cartID)
			if err != nil || expiry == nil || !expiry.Protected {
				t.Fatalf("missing purchase lost expiry protection: %+v err=%v", expiry, err)
			}
			if !bytes.Equal(before, snapshot()) {
				t.Fatal("observing absence changed cart data or items")
			}
			if got := len(fake.callsWithPrefix("")); got != callsBefore+1 {
				t.Fatalf("expected only a status read, got calls: %v", fake.callsWithPrefix(""))
			}

			newID, err := parseUUID(newCart)
			if err != nil {
				t.Fatal(err)
			}
			current, err := testRepo.queries.GetCartByID(ctx, newID)
			if err != nil || current.ErpOrderStatus.String != "aberto" {
				t.Fatalf("current purchase changed: %+v err=%v", current, err)
			}
			selected, err := testRepo.queries.GetCartByEventAndUser(ctx, sqlc.GetCartByEventAndUserParams{
				EventID: current.EventID, PlatformUserID: current.PlatformUserID,
			})
			if err != nil || selected.ID != current.ID {
				t.Fatalf("event lookup did not select current purchase: cart=%v err=%v", selected.ID, err)
			}
			if vip {
				selected, err = testRepo.queries.GetEternalCartByStoreAndHandleForUpdate(ctx,
					sqlc.GetEternalCartByStoreAndHandleForUpdateParams{
						StoreID: current.StoreID, PlatformHandle: current.PlatformHandle,
					})
				if err != nil || selected.ID != current.ID {
					t.Fatalf("VIP lookup did not select current purchase: cart=%v err=%v", selected.ID, err)
				}
			}
		})
	}
}

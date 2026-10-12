//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/erp/erpwrite"
	"livecart/apps/api/internal/integration/providers"
)

type expiryOutcomeERP struct {
	*scriptedERP
	onSet    func()
	onGet    func()
	writeErr error
	readErr  error
}

type cancellationClaimRaceRepository struct {
	erpRepoAdapter
	afterClaim func()
}

func (r cancellationClaimRaceRepository) TransitionCartCancellation(
	ctx context.Context, change erp.CancellationTransition,
) (bool, error) {
	won, err := r.Repository.TransitionCartCancellation(ctx, change)
	if won && err == nil && change.From == erp.OrderStateOpen && change.To == erp.OrderStateCancelled {
		r.afterClaim()
	}
	return won, err
}

func TestExpiryCancellationRechecksPurchaseAfterQuotaWaitBeforePUT(t *testing.T) {
	for _, change := range []string{
		`payment_status='paid',erp_order_state='confirmed'`,
		`status='checkout',erp_order_state='open'`,
		`external_order_id='replacement-sale',erp_order_state='open'`,
	} {
		t.Run(change, func(t *testing.T) {
			fx := seedExpiredCancellation(t, "aberto")
			fake := newScriptedERP()
			svc := newFinalisationService(fake)
			repo := cancellationClaimRaceRepository{
				erpRepoAdapter: erpRepoAdapter{testRepo},
				afterClaim: func() {
					// This mutation runs after the cancellation claim and before
					// the queued callback is dispatched to the provider.
					if _, err := testPool.Exec(t.Context(), `UPDATE carts SET `+change+` WHERE id=$1`, fx.cartID); err != nil {
						t.Fatal(err)
					}
				},
			}
			flow := erp.NewService(repo, svc, zap.NewNop())
			flow.SetWriteLimits(erpwrite.Limits{
				BurstN: 4096, BurstWindow: time.Millisecond,
				SustainedN: 1 << 20, SustWindow: time.Millisecond,
			})
			if err := flow.OnCartExpired(t.Context(), fx.cartID, fx.storeID); !errors.Is(err, erp.ErrCartBusy) {
				t.Fatalf("raced purchase was not deferred: %v", err)
			}
			if len(fake.callsWithPrefix("")) != 0 {
				t.Fatalf("paid/reopened/rebound purchase reached ERP: %v", fake.calls)
			}
		})
	}
}

func (f *expiryOutcomeERP) SetOrderSituacao(ctx context.Context, id string, situation int) error {
	if err := f.scriptedERP.SetOrderSituacao(ctx, id, situation); err != nil {
		return err
	}
	f.mu.Lock()
	f.situacoes[id] = situation
	f.mu.Unlock()
	if f.onSet != nil {
		f.onSet()
	}
	return f.writeErr
}

func (f *expiryOutcomeERP) GetOrderSituacao(ctx context.Context, id string) (int, error) {
	situation, err := f.scriptedERP.GetOrderSituacao(ctx, id)
	if f.onGet != nil {
		f.onGet()
	}
	if f.readErr != nil {
		return providers.SituacaoDesconhecida, f.readErr
	}
	return situation, err
}

func seedExpiredCancellation(t *testing.T, cached string) finFixture {
	t.Helper()
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status='expired',payment_status='pending',
        paid_at=NULL,paid_amount_cents=0,purchase_closed=false,expires_at=now()-interval '3 days',
        external_order_id=$1::text,erp_order_state='open',erp_order_status=$2 WHERE id=$1`, fx.cartID, cached); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE carts SET erp_order_state='cancelled' WHERE id=$1`, fx.cartID)
	})
	return fx
}

func TestExpiryCancellationTimeoutWebhookRetryConverges(t *testing.T) {
	for _, delayedRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("verification_on_retry_%v", delayedRead), func(t *testing.T) {
			fx := seedExpiredCancellation(t, "aberto")
			fake := &expiryOutcomeERP{scriptedERP: newScriptedERP(), writeErr: context.DeadlineExceeded}
			fake.onSet = func() {
				// The remote cancellation succeeded, its webhook arrived, but
				// the PUT response was lost. This is the production ordering.
				if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status='cancelado' WHERE id=$1`, fx.cartID); err != nil {
					t.Fatal(err)
				}
			}
			if delayedRead {
				fake.readErr = context.DeadlineExceeded
			}
			svc := newFinalisationService(fake)
			err := svc.ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID)
			if delayedRead {
				state, _, _, _ := cartERPState(t, fx.cartID)
				if err == nil || state != "open" {
					t.Fatalf("uncertain cancellation must remain retryable: state=%s err=%v", state, err)
				}
				fake.readErr = nil
				err = svc.ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if state, _, _, _ := cartERPState(t, fx.cartID); state != "cancelled" {
				t.Fatalf("remote cancellation failed to converge locally: %s", state)
			}
			if err := svc.ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID); err != nil {
				t.Fatal(err)
			}
			if fake.count("Situacao:") != 1 {
				t.Fatalf("retry re-cancelled the order: %v", fake.callsWithPrefix(""))
			}
		})
	}
}

func TestExpiryCancellationCacheRequiresFreshRemoteProof(t *testing.T) {
	for _, tc := range []struct {
		name      string
		situation int
		readErr   error
		confirmed bool
	}{
		{name: "cancelled", situation: providers.SituacaoCancelada, confirmed: true},
		{name: "reopened", situation: providers.SituacaoAberta},
		{name: "approved", situation: providers.SituacaoAprovada},
		{name: "unknown", situation: providers.SituacaoDesconhecida},
		{name: "unavailable", readErr: context.DeadlineExceeded},
		{name: "permission_denied", readErr: errors.New("permission denied")},
		{name: "confirmed_absent", readErr: providers.ErrOrderNotFound, confirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedExpiredCancellation(t, "cancelado")
			fake := &expiryOutcomeERP{scriptedERP: newScriptedERP(), readErr: tc.readErr}
			fake.situacoes[fx.cartID] = tc.situation
			svc := newFinalisationService(fake)
			err := svc.ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID)
			state, _, _, _ := cartERPState(t, fx.cartID)
			if (err == nil) != tc.confirmed || (state == "cancelled") != tc.confirmed {
				t.Fatalf("cache was trusted without remote proof: state=%s err=%v", state, err)
			}
			if fake.count("GetSituacao:") != 1 || fake.count("Situacao:") != 0 {
				t.Fatalf("reconciliation must only GET: %v", fake.callsWithPrefix(""))
			}
		})
	}
}

func TestExpiryCancellationProofCannotCrossConcurrentPurchaseChanges(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"paid_status", `payment_status='paid'`},
		{"paid_amount", `paid_amount_cents=1000`},
		{"refunded", `payment_status='refunded'`},
		{"closed", `purchase_closed=true`},
		{"reopened", `status='checkout'`},
		{"extended_deadline", `expires_at=now()+interval '1 day'`},
		{"review", `payment_review_required=true`},
		{"never_expires", `never_expires=true`},
		{"binding", `external_order_id='replacement-sale'`},
		{"remote_reopened_webhook", `erp_order_status='aberto'`},
		{"vip", ""},
		{"joined", ""},
		{"account_replaced", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedExpiredCancellation(t, "cancelado")
			fake := &expiryOutcomeERP{scriptedERP: newScriptedERP()}
			fake.situacoes[fx.cartID] = providers.SituacaoCancelada
			fake.onGet = func() {
				query := `UPDATE carts SET ` + tc.change + ` WHERE id=$1`
				switch tc.name {
				case "vip":
					query = `INSERT INTO vip_handles(store_id,platform_handle) SELECT store_id,lower(ltrim(platform_handle,'@')) FROM carts WHERE id=$1`
				case "joined":
					host := seedPaidCart(t, 1, 0)
					query = fmt.Sprintf(`UPDATE carts SET joined_to_cart_id='%s' WHERE id=$1`, host.cartID)
				case "account_replaced":
					query = `UPDATE integrations SET status='disconnected' WHERE store_id=(SELECT store_id FROM carts WHERE id=$1)`
				}
				if _, err := testPool.Exec(t.Context(), query, fx.cartID); err != nil {
					t.Fatal(err)
				}
			}
			err := newFinalisationService(fake).ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID)
			var state string
			if readErr := testPool.QueryRow(t.Context(), `SELECT erp_order_state FROM carts WHERE id=$1`, fx.cartID).Scan(&state); readErr != nil {
				t.Fatal(readErr)
			}
			if !errors.Is(err, erp.ErrCartBusy) || state != "open" || fake.count("Situacao:") != 0 {
				t.Fatalf("stale proof overwrote new purchase: state=%s err=%v calls=%v", state, err, fake.callsWithPrefix(""))
			}
		})
	}
}

func TestExpiryCancellationProtectsPaidReopenedAndJoinedBeforeERP(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"paid", `payment_status='paid'`},
		{"reopened", `status='active'`},
		{"closed", `purchase_closed=true`},
		{"joined", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedExpiredCancellation(t, "cancelado")
			if tc.name == "joined" {
				host := seedExpiredCancellation(t, "cancelado")
				tc.change = fmt.Sprintf(`joined_to_cart_id='%s'`, host.cartID)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET `+tc.change+` WHERE id=$1`, fx.cartID); err != nil {
				t.Fatal(err)
			}
			fake := newScriptedERP()
			if err := newFinalisationService(fake).ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID); err != nil {
				t.Fatal(err)
			}
			if len(fake.callsWithPrefix("")) != 0 {
				t.Fatalf("protected purchase reached ERP: %v", fake.calls)
			}
		})
	}
}

func TestExpiryRecoveryConvergesOldConfirmedCancellationWithoutCancellingLegacySales(t *testing.T) {
	confirmed := seedExpiredCancellation(t, "cancelado")
	open := seedExpiredCancellation(t, "aberto")
	reopened := seedExpiredCancellation(t, "cancelado")
	fake := newScriptedERP()
	fake.situacoes[confirmed.cartID] = providers.SituacaoCancelada
	fake.situacoes[open.cartID] = providers.SituacaoAberta
	fake.situacoes[reopened.cartID] = providers.SituacaoAberta
	svc := newFinalisationService(fake)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	svc.RecoverRecentCartExpiries(ctx)
	if state, _, _, _ := cartERPState(t, confirmed.cartID); state != "cancelled" {
		t.Fatalf("old confirmed cancellation missed by recovery: %s", state)
	}
	for _, fx := range []finFixture{open, reopened} {
		if state, _, _, _ := cartERPState(t, fx.cartID); state != "open" {
			t.Fatalf("legacy live sale was changed: %s", state)
		}
	}
	if fake.count("Situacao:") != 0 || fake.count("GetSituacao:"+open.cartID) != 0 {
		t.Fatalf("recovery interfered with legacy open sales: %v", fake.callsWithPrefix(""))
	}
}

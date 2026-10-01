//go:build integration

package integration

import (
	"errors"
	"testing"

	"livecart/apps/api/internal/erp"
)

func TestCancelCartFromERPReportsFinalisationContention(t *testing.T) {
	requireDB(t)
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	markCartPaid(t, cartID)
	before, _ := cartStatusAndReason(t, cartID)
	release, acquired, err := testRepo.AcquireCartFinalisationLock(t.Context(), cartID)
	if err != nil || !acquired {
		t.Fatalf("acquiring finalisation claim: acquired=%v err=%v", acquired, err)
	}
	defer release()
	cancelled, err := scaleService().CancelCartFromERP(t.Context(), cartID, fx.storeID)
	if cancelled || !errors.Is(err, erp.ErrCartBusy) {
		t.Fatalf("contention misreported as a refund decision: cancelled=%v err=%v", cancelled, err)
	}
	after, _ := cartStatusAndReason(t, cartID)
	if after != before || productStock(t, productID) != 0 {
		t.Fatal("ERP cancellation changed the paid cart or released its inventory during finalisation")
	}
}

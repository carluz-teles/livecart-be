//go:build integration

package live

import "testing"

func TestIncidentReplayMustRespectClosedJoinedPurchase(t *testing.T) {
	svc, input := commentItemFixture(t, 10)
	input.Quantity = 1
	comment := "audit-replay-" + input.ProductID
	first, err := svc.ApplyCommentItem(t.Context(), input, comment)
	if err != nil {
		t.Fatal(err)
	}
	other := input
	other.PlatformUserID = "audit-host"
	other.PlatformHandle = "audit-host"
	host, _, err := svc.getOrCreateCartForItem(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testPool.Exec(t.Context(), `UPDATE carts SET joined_to_cart_id=$2 WHERE id=$1`, first.CartID, host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = testPool.Exec(t.Context(), `UPDATE carts SET payment_status='paid' WHERE id=$1`, host.ID); err != nil {
		t.Fatal(err)
	}
	var closed bool
	if err = testPool.QueryRow(t.Context(), `SELECT purchase_closed FROM carts WHERE id=$1`, first.CartID).Scan(&closed); err != nil || !closed {
		t.Fatalf("fixture not closed %v %v", closed, err)
	}
	replay, err := svc.ApplyCommentItem(t.Context(), input, comment)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Quantity > 0 && !replay.ERPConfirmed && !replay.ERPBlocked {
		t.Fatalf("replay still requests ERP synchronization on closed purchase: quantity=%d confirmed=%v blocked=%v", replay.Quantity, replay.ERPConfirmed, replay.ERPBlocked)
	}
}

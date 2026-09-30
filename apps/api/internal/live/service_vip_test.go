//go:build integration

package live

import (
	"context"
	"errors"
	"testing"
)

type failingVipChecker struct{ err error }

func (c failingVipChecker) IsVipHandle(context.Context, string, string) (bool, error) {
	return false, c.err
}

func TestVIPLookupFailureDoesNotCreateOrdinaryCart(t *testing.T) {
	svc, input := commentItemFixture(t, 10)
	want := errors.New("membership lookup unavailable")
	svc.vipChecker = failingVipChecker{err: want}
	_, _, err := svc.getOrCreateCartForItem(t.Context(), input)
	if !errors.Is(err, want) {
		t.Fatalf("expected retryable membership error, got %v", err)
	}
	var carts int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM carts WHERE event_id=$1`, input.EventID).Scan(&carts); err != nil {
		t.Fatal(err)
	}
	if carts != 0 {
		t.Fatalf("created %d carts while membership was unknown", carts)
	}
	// Retrying after recovery must still accept the buyer's request.
	svc.vipChecker = nil
	if _, _, err := svc.getOrCreateCartForItem(t.Context(), input); err != nil {
		t.Fatal(err)
	}
}

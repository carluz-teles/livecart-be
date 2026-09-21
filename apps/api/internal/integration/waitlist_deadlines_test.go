//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

type deadlineCloseScheduler struct {
	eventID string
	at      time.Time
	calls   int
}

func (s *deadlineCloseScheduler) ScheduleEventWaitlistClose(_ context.Context, eventID string, at time.Time) error {
	s.eventID, s.at = eventID, at
	s.calls++
	return nil
}

func TestWaitlistDeadlineOldTaskRearmsCurrentCartDate(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWaitlistCloseFixture(t)
	var deadline time.Time
	if err := testPool.QueryRow(ctx, `UPDATE carts SET expires_at = now() + interval '2 hours'
	    WHERE id = $1 RETURNING expires_at`, f.carts["waiting"]).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	scheduler := &deadlineCloseScheduler{}
	svc := &Service{repo: testRepo, logger: zap.NewNop(), waitlistCloseSched: scheduler}
	if err := svc.ArmEventWaitlistClose(ctx, f.eventID); err != nil {
		t.Fatal(err)
	}
	if scheduler.calls != 1 || !scheduler.at.Equal(deadline) {
		t.Fatalf("armed deadline = %v, calls = %d, want %v", scheduler.at, scheduler.calls, deadline)
	}
	if err := svc.RunEventWaitlistClose(ctx, f.eventID); err != nil {
		t.Fatal(err)
	}
	if got := waitlistStatusByCart(t, f.carts["waiting"]); got != "waiting" {
		t.Fatalf("old task expired future waitlist: %s", got)
	}
	if scheduler.calls != 2 || !scheduler.at.Equal(deadline) {
		t.Fatalf("old task failed to rearm current deadline: %+v", scheduler)
	}
}

func TestWaitlistDeadlinePreservesVIPPaymentAndReview(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, update string
	}{
		{"VIP", "never_expires = true"},
		{"paid", "payment_status = 'paid'"},
		{"refunded", "payment_status = 'refunded'"},
		{"payment review", "payment_review_required = true"},
		{"future deadline", "expires_at = now() + interval '1 day'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedWaitlistCloseFixture(t)
			cartID := f.carts["waiting"]
			// SQL fragments above are fixed test data, never external input.
			if _, err := testPool.Exec(ctx, "UPDATE carts SET "+tc.update+" WHERE id = $1", cartID); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := testPool.QueryRow(ctx, `SELECT status FROM waitlist_items WHERE cart_id = $1`, cartID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := testRepo.ExpireEventWaitlist(ctx, f.eventID); err != nil {
				t.Fatal(err)
			}
			// Payment may have atomically cancelled the pending balance already.
			// The timer itself may never expire a protected cart's waitlist.
			if got := waitlistStatusByCart(t, cartID); got != before {
				t.Fatalf("timer changed protected waitlist from %s to %s", before, got)
			}
			result, err := testRepo.ExpireCartAndReleaseStock(ctx, cartID, f.storeID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Eligible {
				t.Fatal("protected cart expired")
			}
		})
	}
}

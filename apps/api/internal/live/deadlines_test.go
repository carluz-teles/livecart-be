//go:build integration

package live

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type deadlineFixture struct {
	storeID, eventID, productID string
}

func seedDeadlineFixture(t *testing.T, x, y int) deadlineFixture {
	t.Helper()
	ctx := context.Background()
	f := deadlineFixture{storeID: seedWindowStore(t, ctx, fmt.Sprintf("deadline-%d", time.Now().UnixNano()))}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO live_events (store_id, title, status, ends_at, close_cart_on_event_end,
		    cart_expiration_minutes, waitlist_notified_ttl_minutes)
		VALUES ($1, 'Deadline contract', 'active', now() + interval '1 day', true, $2, $3)
		RETURNING id::text`, f.storeID, x, y).Scan(&f.eventID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO products (store_id, name, keyword, price, stock, external_source)
		VALUES ($1, 'Deadline product', 'DL', 1000, 0, 'manual') RETURNING id::text`,
		f.storeID).Scan(&f.productID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f deadlineFixture) cart(t *testing.T, label, waitingStatus string, vip bool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		    status, payment_status, never_expires)
		VALUES ($1, $2, $2, $3, 12345, 'active', 'pending', $4) RETURNING id::text`,
		f.eventID, label, fmt.Sprintf("deadline-%s-%s", f.eventID, label), vip).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if waitingStatus != "" {
		pending := 0
		if waitingStatus == "waiting" {
			pending = 1
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO cart_items(cart_id, product_id, quantity, waitlisted_quantity, unit_price)
		    VALUES ($1, $2, 1, $3, 1000)`, id, f.productID, pending); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `
			INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle,
			    cart_id, quantity, position, status, notified_at)
			VALUES ($1, $2, $3, $3, $4, 1, 1, $5::text,
			    CASE WHEN $5::text <> 'waiting' THEN now() - interval '1 second' END)`,
			f.eventID, f.productID, label, id, waitingStatus); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func readDeadline(t *testing.T, id string) *time.Time {
	t.Helper()
	var deadline *time.Time
	if err := testPool.QueryRow(context.Background(), `SELECT expires_at FROM carts WHERE id = $1`, id).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	return deadline
}

func assertDeadline(t *testing.T, id string, want time.Time) {
	t.Helper()
	got := readDeadline(t, id)
	if got == nil || !got.Equal(want) {
		t.Fatalf("deadline = %v, want %v", got, want)
	}
}

func (f deadlineFixture) close(t *testing.T) time.Time {
	t.Helper()
	if _, err := testRepo.EndEvent(context.Background(), f.eventID, f.storeID); err != nil {
		t.Fatal(err)
	}
	var closedAt time.Time
	if err := testPool.QueryRow(context.Background(), `SELECT commercial_closed_at FROM live_events WHERE id = $1`, f.eventID).Scan(&closedAt); err != nil {
		t.Fatal(err)
	}
	return closedAt
}

func TestDeadlineEligibilityFrozenAtCommercialClose(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedDeadlineFixture(t, 120, 60)
	ordinary := f.cart(t, "ordinary", "", false)
	waiting := f.cart(t, "waiting", "waiting", false)
	fulfilled := f.cart(t, "fulfilled-before", "fulfilled", false)
	vip := f.cart(t, "vip", "waiting", true)
	e := f.close(t)

	// Fulfillment between the close transaction and delayed finalization must
	// preserve the eligibility already frozen at E.
	if _, err := testPool.Exec(ctx, `UPDATE waitlist_items SET status = 'fulfilled', notified_at = now() WHERE cart_id = $1`, waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := testRepo.FinalizeCartsByEvent(ctx, f.eventID); err != nil {
		t.Fatal(err)
	}
	assertDeadline(t, ordinary, e.Add(120*time.Minute))
	assertDeadline(t, fulfilled, e.Add(120*time.Minute))
	assertDeadline(t, waiting, e.Add(180*time.Minute))
	if got := readDeadline(t, vip); got != nil {
		t.Fatalf("VIP received deadline: %v", got)
	}

	// Event metadata edits and repeated close/finalize cannot restart time.
	if _, err := testPool.Exec(ctx, `UPDATE live_events SET title = 'Edited', ends_at = now() + interval '7 days' WHERE id = $1`, f.eventID); err != nil {
		t.Fatal(err)
	}
	if again := f.close(t); !again.Equal(e) {
		t.Fatalf("commercial close changed: %v -> %v", e, again)
	}
	if n, err := testRepo.FinalizeCartsByEvent(ctx, f.eventID); err != nil || n != 0 {
		t.Fatalf("repeat finalization = (%d, %v), want (0, nil)", n, err)
	}
	assertDeadline(t, waiting, e.Add(180*time.Minute))
}

func TestDeadlineConfigChangesNeverShortenOrDoubleGrant(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedDeadlineFixture(t, 120, 60)
	ordinary := f.cart(t, "ordinary", "", false)
	waiting := f.cart(t, "waiting", "waiting", false)
	e := f.close(t)
	if _, err := testRepo.FinalizeCartsByEvent(ctx, f.eventID); err != nil {
		t.Fatal(err)
	}
	// Leaving the queue after E does not revoke Y or its future increases.
	if _, err := testPool.Exec(ctx, `UPDATE waitlist_items SET status = 'cancelled', cancelled_at = now() WHERE cart_id = $1`, waiting); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                                  string
		x, y, ordinaryMinutes, waitingMinutes int
	}{
		{"increase X and Y", 180, 90, 180, 270},
		{"decrease both", 60, 0, 180, 270},
		{"restore same configuration", 180, 90, 180, 270},
		{"increase only Y", 180, 120, 180, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := testRepo.ApplyEventDeadlineSettings(ctx, f.eventID, f.storeID, &tc.x, &tc.y); err != nil {
				t.Fatal(err)
			}
			assertDeadline(t, ordinary, e.Add(time.Duration(tc.ordinaryMinutes)*time.Minute))
			assertDeadline(t, waiting, e.Add(time.Duration(tc.waitingMinutes)*time.Minute))
		})
	}

	// A terminal cart keeps its date and status even if the configuration grows.
	if _, err := testPool.Exec(ctx, `UPDATE carts SET status = 'cancelled' WHERE id = $1`, waiting); err != nil {
		t.Fatal(err)
	}
	x, y := 400, 400
	if _, err := testRepo.ApplyEventDeadlineSettings(ctx, f.eventID, f.storeID, &x, &y); err != nil {
		t.Fatal(err)
	}
	assertDeadline(t, waiting, e.Add(300*time.Minute))
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM carts WHERE id = $1`, waiting).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("terminal cart status = %q, err %v", status, err)
	}
}

func TestDeadlineYBoundariesAndLateClose(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	for _, y := range []int{0, 43200} {
		t.Run(fmt.Sprintf("Y_%d", y), func(t *testing.T) {
			f := seedDeadlineFixture(t, 120, y)
			cartID := f.cart(t, "waiting", "waiting", false)
			var scheduled time.Time
			if err := testPool.QueryRow(ctx, `UPDATE live_events SET ends_at = now() - interval '1 hour' WHERE id = $1 RETURNING ends_at`, f.eventID).Scan(&scheduled); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `UPDATE waitlist_items SET created_at = $2::timestamptz - interval '1 minute' WHERE cart_id = $1`, cartID, scheduled); err != nil {
				t.Fatal(err)
			}
			if e := f.close(t); !e.Equal(scheduled) {
				t.Fatalf("late close restarted E: %v, want %v", e, scheduled)
			}
			if _, err := testRepo.FinalizeCartsByEvent(ctx, f.eventID); err != nil {
				t.Fatal(err)
			}
			assertDeadline(t, cartID, scheduled.Add(time.Duration(120+y)*time.Minute))
		})
	}
}

func TestDeadlineLegacyUnknownEligibilityPreservesEvidence(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedDeadlineFixture(t, 120, 60)
	cartID := f.cart(t, "legacy", "", false)
	var original time.Time
	if err := testPool.QueryRow(ctx, `UPDATE carts SET status = 'checkout',
	    expires_at = now() + interval '2 hours', deadline_config_base_at = now() + interval '2 hours',
	    deadline_config_x_minutes = 120, deadline_config_y_minutes = 60
	    WHERE id = $1 RETURNING expires_at`, cartID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, x := range []int{180, 60, 180} {
		y := 43200
		if _, err := testRepo.ApplyEventDeadlineSettings(ctx, f.eventID, f.storeID, &x, &y); err != nil {
			t.Fatal(err)
		}
		assertDeadline(t, cartID, original.Add(time.Hour))
	}
	var e *time.Time
	var eligible *bool
	if err := testPool.QueryRow(ctx, `SELECT e.commercial_closed_at, c.waitlist_extra_eligible
	    FROM carts c JOIN live_events e ON e.id = c.event_id WHERE c.id = $1`, cartID).Scan(&e, &eligible); err != nil {
		t.Fatal(err)
	}
	if e != nil || eligible != nil {
		t.Fatalf("invented legacy history: E=%v eligible=%v", e, eligible)
	}
}

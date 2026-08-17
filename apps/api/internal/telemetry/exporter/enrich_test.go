package exporter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/idempotency"
)

const testCartID = "11111111-1111-1111-1111-111111111111"

// fakeEnrichQueries is a hand-rolled fake of enrichQueries — no mocking
// framework in this codebase's Go tests, matches the fakeSender pattern in
// listeners_test.go. Each field is a canned response/error for its query.
type fakeEnrichQueries struct {
	cart           sqlc.Cart
	cartErr        error
	items          []sqlc.ListCartItemsRow
	itemsErr       error
	ledgerEntry    sqlc.BillingLedgerEntry
	ledgerErr      error
	correlation    sqlc.FindCommentCartCorrelationRow
	correlationErr error
}

func (f *fakeEnrichQueries) GetCartByID(_ context.Context, _ pgtype.UUID) (sqlc.Cart, error) {
	return f.cart, f.cartErr
}

func (f *fakeEnrichQueries) ListCartItems(_ context.Context, _ pgtype.UUID) ([]sqlc.ListCartItemsRow, error) {
	return f.items, f.itemsErr
}

func (f *fakeEnrichQueries) GetLedgerSaleEntry(_ context.Context, _ pgtype.UUID) (sqlc.BillingLedgerEntry, error) {
	return f.ledgerEntry, f.ledgerErr
}

func (f *fakeEnrichQueries) FindCommentCartCorrelation(_ context.Context, _ string) (sqlc.FindCommentCartCorrelationRow, error) {
	return f.correlation, f.correlationErr
}

func newTestEnricher(q enrichQueries) (*Enricher, *observer.ObservedLogs) {
	core, logs := observer.New(zap.WarnLevel)
	log := zap.New(core)
	return NewEnricher(q, log), logs
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	id, err := idempotency.ParseUUID(s)
	if err != nil {
		t.Fatalf("parsing test uuid %q: %v", s, err)
	}
	return id
}

func TestEnricher_CartSnapshot(t *testing.T) {
	t.Parallel()

	openedAt := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	paidAt := openedAt.Add(90 * time.Second)

	t.Run("populates discount/shipping/duration from the cart row", func(t *testing.T) {
		t.Parallel()

		cart := sqlc.Cart{
			CouponDiscountCents: 500,
		}
		cart.ShippingCostCents.Int64, cart.ShippingCostCents.Valid = 1200, true
		cart.InitialSnapshotTakenAt.Time, cart.InitialSnapshotTakenAt.Valid = openedAt, true
		cart.PaidAt.Time, cart.PaidAt.Valid = paidAt, true

		enricher, _ := newTestEnricher(&fakeEnrichQueries{cart: cart})

		got, ok := enricher.CartSnapshot(t.Context(), testCartID)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		want := CartSnapshot{DiscountCents: 500, ShippingCents: 1200, CheckoutDurationMs: 90000}
		if got != want {
			t.Errorf("snapshot = %+v, want %+v", got, want)
		}
	})

	t.Run("checkout duration stays zero without an initial snapshot timestamp", func(t *testing.T) {
		t.Parallel()

		cart := sqlc.Cart{CouponDiscountCents: 0}
		cart.PaidAt.Time, cart.PaidAt.Valid = paidAt, true
		// InitialSnapshotTakenAt left invalid — cart paid without ever opening
		// the public checkout page (see enrich.go's KNOWN_LIMITATION doc).

		enricher, _ := newTestEnricher(&fakeEnrichQueries{cart: cart})

		got, ok := enricher.CartSnapshot(t.Context(), testCartID)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if got.CheckoutDurationMs != 0 {
			t.Errorf("CheckoutDurationMs = %d, want 0", got.CheckoutDurationMs)
		}
	})

	t.Run("returns not-ok on an invalid cart id", func(t *testing.T) {
		t.Parallel()

		enricher, logs := newTestEnricher(&fakeEnrichQueries{})

		_, ok := enricher.CartSnapshot(t.Context(), "not-a-uuid")
		if ok {
			t.Errorf("ok = true, want false for an invalid cart id")
		}
		if logs.FilterMessageSnippet("invalid cart_id").Len() != 1 {
			t.Errorf("expected exactly one 'invalid cart_id' warning")
		}
	})

	t.Run("returns not-ok and logs when the query fails", func(t *testing.T) {
		t.Parallel()

		enricher, logs := newTestEnricher(&fakeEnrichQueries{cartErr: errors.New("db unavailable")})

		_, ok := enricher.CartSnapshot(t.Context(), testCartID)
		if ok {
			t.Errorf("ok = true, want false on query failure")
		}
		if logs.FilterMessageSnippet("GetCartByID failed").Len() != 1 {
			t.Errorf("expected exactly one 'GetCartByID failed' warning")
		}
	})
}

func TestEnricher_CartItemBreakdown(t *testing.T) {
	t.Parallel()

	t.Run("builds one payload per cart item with revenue = quantity * unit_price", func(t *testing.T) {
		t.Parallel()

		row := sqlc.ListCartItemsRow{ProductName: "Camiseta P"}
		row.ProductID = mustUUID(t, "22222222-2222-2222-2222-222222222222")
		row.Quantity.Int32, row.Quantity.Valid = 3, true
		row.UnitPrice.Int64, row.UnitPrice.Valid = 1000, true

		enricher, _ := newTestEnricher(&fakeEnrichQueries{items: []sqlc.ListCartItemsRow{row}})

		got := enricher.CartItemBreakdown(t.Context(), testCartID, "live-1", "store-1", 1700000000000)
		if len(got) != 1 {
			t.Fatalf("len(got) = %d, want 1", len(got))
		}
		want := LiveCommerceCartItemPayload{
			EventType:      eventTypeLiveCommerceCartItem,
			LiveEventID:    "live-1",
			StoreID:        "store-1",
			CartID:         testCartID,
			ProductID:      "22222222-2222-2222-2222-222222222222",
			ProductName:    "Camiseta P",
			Quantity:       3,
			UnitPriceCents: 1000,
			RevenueCents:   3000,
			Timestamp:      1700000000000,
		}
		if got[0] != want {
			t.Errorf("item = %+v, want %+v", got[0], want)
		}
	})

	t.Run("returns nil on an invalid cart id", func(t *testing.T) {
		t.Parallel()

		enricher, _ := newTestEnricher(&fakeEnrichQueries{})

		got := enricher.CartItemBreakdown(t.Context(), "not-a-uuid", "live-1", "store-1", 0)
		if got != nil {
			t.Errorf("got = %+v, want nil", got)
		}
	})

	t.Run("returns nil and logs when the query fails", func(t *testing.T) {
		t.Parallel()

		enricher, logs := newTestEnricher(&fakeEnrichQueries{itemsErr: errors.New("db unavailable")})

		got := enricher.CartItemBreakdown(t.Context(), testCartID, "live-1", "store-1", 0)
		if got != nil {
			t.Errorf("got = %+v, want nil", got)
		}
		if logs.FilterMessageSnippet("ListCartItems failed").Len() != 1 {
			t.Errorf("expected exactly one 'ListCartItems failed' warning")
		}
	})
}

func TestEnricher_LedgerPlan(t *testing.T) {
	t.Parallel()

	t.Run("returns the sale entry's plan and billable flag", func(t *testing.T) {
		t.Parallel()

		enricher, _ := newTestEnricher(&fakeEnrichQueries{
			ledgerEntry: sqlc.BillingLedgerEntry{Plan: "grow", Billable: true},
		})

		got, ok := enricher.LedgerPlan(t.Context(), testCartID)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		want := LedgerInfo{Plan: "grow", Billable: true}
		if got != want {
			t.Errorf("info = %+v, want %+v", got, want)
		}
	})

	t.Run("returns not-ok on an invalid cart id", func(t *testing.T) {
		t.Parallel()

		enricher, _ := newTestEnricher(&fakeEnrichQueries{})

		_, ok := enricher.LedgerPlan(t.Context(), "not-a-uuid")
		if ok {
			t.Errorf("ok = true, want false for an invalid cart id")
		}
	})

	t.Run("returns not-ok and logs when no sale entry exists", func(t *testing.T) {
		t.Parallel()

		enricher, logs := newTestEnricher(&fakeEnrichQueries{ledgerErr: errors.New("no rows")})

		_, ok := enricher.LedgerPlan(t.Context(), testCartID)
		if ok {
			t.Errorf("ok = true, want false on query failure")
		}
		if logs.FilterMessageSnippet("GetLedgerSaleEntry failed").Len() != 1 {
			t.Errorf("expected exactly one 'GetLedgerSaleEntry failed' warning")
		}
	})
}

func TestEnricher_CommentCartCorrelation(t *testing.T) {
	t.Parallel()

	commentCreatedAt := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	t.Run("converted_to_cart=true with cart_id and time_to_cart_ms when a cart matched", func(t *testing.T) {
		t.Parallel()

		row := sqlc.FindCommentCartCorrelationRow{
			Platform:       "instagram",
			PlatformUserID: "ig-buyer-1",
		}
		row.EventID = mustUUID(t, "11111111-1111-1111-1111-111111111111")
		row.StoreID = mustUUID(t, "22222222-2222-2222-2222-222222222222")
		row.SessionID = mustUUID(t, "33333333-3333-3333-3333-333333333333")
		row.HasPurchaseIntent.Bool, row.HasPurchaseIntent.Valid = true, true
		row.MatchedProductID = mustUUID(t, "44444444-4444-4444-4444-444444444444")
		row.Result.String, row.Result.Valid = "added_to_cart", true
		row.CommentCreatedAt.Time, row.CommentCreatedAt.Valid = commentCreatedAt, true
		row.CartID = mustUUID(t, "55555555-5555-5555-5555-555555555555")
		row.ItemCreatedAt.Time, row.ItemCreatedAt.Valid = commentCreatedAt.Add(45*time.Second), true

		enricher, _ := newTestEnricher(&fakeEnrichQueries{correlation: row})

		got, ok := enricher.CommentCartCorrelation(t.Context(), "platform-comment-1")
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		want := CommentCorrelation{
			LiveEventID:       "11111111-1111-1111-1111-111111111111",
			StoreID:           "22222222-2222-2222-2222-222222222222",
			SessionID:         "33333333-3333-3333-3333-333333333333",
			Platform:          "instagram",
			PlatformUserID:    "ig-buyer-1",
			HasPurchaseIntent: true,
			MatchedProductID:  "44444444-4444-4444-4444-444444444444",
			Result:            "added_to_cart",
			ConvertedToCart:   true,
			CartID:            "55555555-5555-5555-5555-555555555555",
			TimeToCartMs:      45000,
		}
		if got != want {
			t.Errorf("correlation = %+v, want %+v", got, want)
		}
	})

	t.Run("converted_to_cart=false with no cart_id when no cart matched the window", func(t *testing.T) {
		t.Parallel()

		row := sqlc.FindCommentCartCorrelationRow{
			Platform:       "instagram",
			PlatformUserID: "ig-buyer-2",
		}
		row.EventID = mustUUID(t, "11111111-1111-1111-1111-111111111111")
		row.HasPurchaseIntent.Bool, row.HasPurchaseIntent.Valid = true, true
		row.Result.String, row.Result.Valid = "added_to_cart", true
		// CartID left invalid — the query's LEFT JOIN found no matching cart.

		enricher, _ := newTestEnricher(&fakeEnrichQueries{correlation: row})

		got, ok := enricher.CommentCartCorrelation(t.Context(), "platform-comment-2")
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if got.ConvertedToCart {
			t.Errorf("ConvertedToCart = true, want false")
		}
		if got.CartID != "" {
			t.Errorf("CartID = %q, want empty", got.CartID)
		}
		if got.TimeToCartMs != 0 {
			t.Errorf("TimeToCartMs = %d, want 0", got.TimeToCartMs)
		}
	})

	t.Run("returns not-ok and logs when the query fails (e.g. comment not persisted yet)", func(t *testing.T) {
		t.Parallel()

		enricher, logs := newTestEnricher(&fakeEnrichQueries{correlationErr: errors.New("no rows in result set")})

		_, ok := enricher.CommentCartCorrelation(t.Context(), "platform-comment-3")
		if ok {
			t.Errorf("ok = true, want false on query failure")
		}
		if logs.FilterMessageSnippet("FindCommentCartCorrelation failed").Len() != 1 {
			t.Errorf("expected exactly one 'FindCommentCartCorrelation failed' warning")
		}
	})
}

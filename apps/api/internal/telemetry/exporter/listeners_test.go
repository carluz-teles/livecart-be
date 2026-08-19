package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/events"
)

// fakeSender records every event SendEvent was called with, and optionally
// returns a canned error so send()'s failure path can be exercised without a
// real HTTP call.
type fakeSender struct {
	sent []any
	err  error
}

func (f *fakeSender) SendEvent(_ context.Context, event any) error {
	f.sent = append(f.sent, event)
	return f.err
}

func newTestListeners(sender eventSender, enabled bool) (*Listeners, *observer.ObservedLogs) {
	core, logs := observer.New(zap.WarnLevel)
	log := zap.New(core)
	return NewListeners(sender, Config{Enabled: enabled, Environment: "staging"}, log), logs
}

func envelopeWithPayload(t *testing.T, name events.Name, payload any) events.Envelope {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshaling test payload: %v", err)
	}

	return events.Envelope{
		EventID:     "evt-1",
		Name:        name,
		Source:      events.Source("pagarme"),
		OccurredAt:  time.Unix(1700000000, 0).UTC(),
		LiveEventID: "live-1",
		Payload:     raw,
	}
}

func TestListeners_Dispatch(t *testing.T) {
	t.Run("no-ops when the exporter is disabled", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, false)

		listeners.Dispatch(t.Context(), envelopeWithPayload(t, events.EventEventCreated, map[string]string{"store_id": "store-1"}))

		if len(sender.sent) != 0 {
			t.Errorf("sent = %d events, want 0 when disabled", len(sender.sent))
		}
	})

	t.Run("ignores facts it doesn't own", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)

		listeners.Dispatch(t.Context(), envelopeWithPayload(t, events.Name("stock.reserved"), map[string]string{}))

		if len(sender.sent) != 0 {
			t.Errorf("sent = %d events, want 0 for an unrelated fact", len(sender.sent))
		}
	})

	t.Run("routes event.created to OnEventCreated", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)

		listeners.Dispatch(t.Context(), envelopeWithPayload(t, events.EventEventCreated, map[string]string{"store_id": "store-1"}))

		if len(sender.sent) != 1 {
			t.Fatalf("sent = %d events, want 1", len(sender.sent))
		}
		got, ok := sender.sent[0].(LiveCommerceEventPayload)
		if !ok {
			t.Fatalf("sent[0] type = %T, want LiveCommerceEventPayload", sender.sent[0])
		}
		if got.Action != "created" || got.StoreID != "store-1" {
			t.Errorf("payload = %+v, want action=created store_id=store-1", got)
		}
	})
}

func TestListeners_OnEventCreated(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.EventEventCreated, map[string]string{"store_id": "store-1"})

	listeners.OnEventCreated(t.Context(), env)

	want := LiveCommerceEventPayload{
		EventType:   eventTypeLiveCommerceEvent,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		Action:      "created",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnEventEnded(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.EventEventEnded, map[string]string{"store_id": "store-1"})

	listeners.OnEventEnded(t.Context(), env)

	want := LiveCommerceEventPayload{
		EventType:   eventTypeLiveCommerceEvent,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		Action:      "ended",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnSessionCreated(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.SessionCreated, map[string]string{"session_id": "sess-1", "platform": "instagram"})

	listeners.OnSessionCreated(t.Context(), env)

	want := LiveCommerceEventPayload{
		EventType:   eventTypeLiveCommerceEvent,
		Environment: "staging",
		LiveEventID: "live-1",
		SessionID:   "sess-1",
		Platform:    "instagram",
		Action:      "created",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnSessionEnded(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.SessionEnded, map[string]string{"session_id": "sess-1"})

	listeners.OnSessionEnded(t.Context(), env)

	want := LiveCommerceEventPayload{
		EventType:   eventTypeLiveCommerceEvent,
		Environment: "staging",
		LiveEventID: "live-1",
		SessionID:   "sess-1",
		Action:      "ended",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCartPaid(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.CartPaid, map[string]any{
		"cart_id":        "cart-1",
		"store_id":       "store-1",
		"payment_id":     "pay-1",
		"payment_method": "credit_card",
		"gmv_cents":      12345,
	})

	listeners.OnCartPaid(t.Context(), env)

	if len(sender.sent) != 2 {
		t.Fatalf("sent = %d events, want 2 (cart + payment)", len(sender.sent))
	}

	cart, ok := sender.sent[0].(LiveCommerceCartPayload)
	if !ok {
		t.Fatalf("sent[0] type = %T, want LiveCommerceCartPayload", sender.sent[0])
	}
	wantCart := LiveCommerceCartPayload{
		EventType:     eventTypeLiveCommerceCart,
		Environment:   "staging",
		LiveEventID:   "live-1",
		StoreID:       "store-1",
		CartID:        "cart-1",
		Status:        "paid",
		GMVCents:      12345,
		PaymentMethod: "credit_card",
		Provider:      "pagarme",
		Timestamp:     env.OccurredAt.UnixMilli(),
	}
	if cart != wantCart {
		t.Errorf("cart payload = %+v, want %+v", cart, wantCart)
	}

	payment, ok := sender.sent[1].(LiveCommercePaymentPayload)
	if !ok {
		t.Fatalf("sent[1] type = %T, want LiveCommercePaymentPayload", sender.sent[1])
	}
	wantPayment := LiveCommercePaymentPayload{
		EventType:   eventTypeLiveCommercePayment,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		PaymentID:   "pay-1",
		Provider:    "pagarme",
		Method:      "credit_card",
		Outcome:     "approved",
		AmountCents: 12345,
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	if payment != wantPayment {
		t.Errorf("payment payload = %+v, want %+v", payment, wantPayment)
	}
}

func TestListeners_OnCartPaid_withEnricher(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)

	cart := sqlc.Cart{CouponDiscountCents: 300}
	cart.ShippingCostCents.Int64, cart.ShippingCostCents.Valid = 800, true
	item := sqlc.ListCartItemsRow{ProductName: "Caneca"}
	item.Quantity.Int32, item.Quantity.Valid = 2, true
	item.UnitPrice.Int64, item.UnitPrice.Valid = 1500, true

	enricher, _ := newTestEnricher(&fakeEnrichQueries{cart: cart, items: []sqlc.ListCartItemsRow{item}})
	listeners.SetEnricher(enricher)

	env := envelopeWithPayload(t, events.CartPaid, map[string]any{
		"cart_id": testCartID, "store_id": "store-1", "payment_id": "pay-1",
		"payment_method": "credit_card", "gmv_cents": 3000,
	})

	listeners.OnCartPaid(t.Context(), env)

	if len(sender.sent) != 3 {
		t.Fatalf("sent = %d events, want 3 (cart + payment + 1 item)", len(sender.sent))
	}

	cartPayload, ok := sender.sent[0].(LiveCommerceCartPayload)
	if !ok {
		t.Fatalf("sent[0] type = %T, want LiveCommerceCartPayload", sender.sent[0])
	}
	if cartPayload.DiscountCents != 300 || cartPayload.ShippingCents != 800 {
		t.Errorf("cart payload = %+v, want DiscountCents=300 ShippingCents=800", cartPayload)
	}

	itemPayload, ok := sender.sent[2].(LiveCommerceCartItemPayload)
	if !ok {
		t.Fatalf("sent[2] type = %T, want LiveCommerceCartItemPayload", sender.sent[2])
	}
	if itemPayload.RevenueCents != 3000 || itemPayload.ProductName != "Caneca" {
		t.Errorf("item payload = %+v, want RevenueCents=3000 ProductName=Caneca", itemPayload)
	}
}

func TestListeners_OnPaymentFailed(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.PaymentFailed, map[string]any{
		"cart_id":        "cart-1",
		"store_id":       "store-1",
		"payment_id":     "pay-1",
		"payment_method": "pix",
	})

	listeners.OnPaymentFailed(t.Context(), env)

	want := LiveCommercePaymentPayload{
		EventType:   eventTypeLiveCommercePayment,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		PaymentID:   "pay-1",
		Provider:    "pagarme",
		Method:      "pix",
		Outcome:     "rejected",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCartCheckoutArmed(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.CartCheckoutArmed, map[string]any{"cart_id": "cart-1"})

	listeners.OnCartCheckoutArmed(t.Context(), env)

	want := LiveCommerceCartPayload{
		EventType:   eventTypeLiveCommerceCart,
		Environment: "staging",
		LiveEventID: "live-1",
		CartID:      "cart-1",
		Status:      "checkout_armed",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCartItemAdded(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.CartItemAdded, map[string]any{"cart_id": "cart-1", "product_id": "prod-1", "quantity": 2})

	listeners.OnCartItemAdded(t.Context(), env)

	want := LiveCommerceCartPayload{
		EventType:   eventTypeLiveCommerceCart,
		Environment: "staging",
		LiveEventID: "live-1",
		CartID:      "cart-1",
		Status:      "item_added",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnERPFinalizationFailed(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.ERPFinalizationFailed, map[string]any{
		"store_id":          "store-1",
		"cart_id":           "cart-1",
		"provider":          "tiny",
		"reason":            "erp timeout",
		"external_order_id": "ext-1",
	})

	listeners.OnERPFinalizationFailed(t.Context(), env)

	want := LiveCommerceOpsPayload{
		EventType:    eventTypeLiveCommerceOps,
		Environment:  "staging",
		LiveEventID:  "live-1",
		StoreID:      "store-1",
		CartID:       "cart-1",
		ErrorType:    "erp.finalization_failed",
		Provider:     "tiny",
		ErrorMessage: "erp timeout",
		Timestamp:    env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnNotificationFailed(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.NotificationFailed, map[string]any{
		"store_id": "store-1",
		"channel":  "whatsapp",
		"cart_id":  "cart-1",
		"error":    "provider timeout",
	})

	listeners.OnNotificationFailed(t.Context(), env)

	want := LiveCommerceOpsPayload{
		EventType:    eventTypeLiveCommerceOps,
		Environment:  "staging",
		LiveEventID:  "live-1",
		StoreID:      "store-1",
		CartID:       "cart-1",
		ErrorType:    "notification.failed",
		Channel:      "whatsapp",
		ErrorMessage: "provider timeout",
		Timestamp:    env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCartExpired(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.CartExpired, map[string]any{"cart_id": "cart-1", "store_id": "store-1"})

	listeners.OnCartExpired(t.Context(), env)

	want := LiveCommerceCartPayload{
		EventType:   eventTypeLiveCommerceCart,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		Status:      "expired",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCartCancelled(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.CartCancelled, map[string]any{
		"cart_id": "cart-1", "store_id": "store-1", "reason": "store_cancelled",
	})

	listeners.OnCartCancelled(t.Context(), env)

	want := LiveCommerceCartPayload{
		EventType:   eventTypeLiveCommerceCart,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		Status:      "cancelled",
		Reason:      "store_cancelled",
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCartRefunded(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.CartRefunded, map[string]any{
		"cart_id": "cart-1", "store_id": "store-1", "payment_id": "pay-1",
		"payment_method": "pix", "gmv_cents": 5000,
	})

	listeners.OnCartRefunded(t.Context(), env)

	if len(sender.sent) != 2 {
		t.Fatalf("sent = %d events, want 2 (cart + payment)", len(sender.sent))
	}

	cart, ok := sender.sent[0].(LiveCommerceCartPayload)
	if !ok {
		t.Fatalf("sent[0] type = %T, want LiveCommerceCartPayload", sender.sent[0])
	}
	wantCart := LiveCommerceCartPayload{
		EventType:     eventTypeLiveCommerceCart,
		Environment:   "staging",
		LiveEventID:   "live-1",
		StoreID:       "store-1",
		CartID:        "cart-1",
		Status:        "refunded",
		GMVCents:      5000,
		PaymentMethod: "pix",
		Provider:      "pagarme",
		Timestamp:     env.OccurredAt.UnixMilli(),
	}
	if cart != wantCart {
		t.Errorf("cart payload = %+v, want %+v", cart, wantCart)
	}

	payment, ok := sender.sent[1].(LiveCommercePaymentPayload)
	if !ok {
		t.Fatalf("sent[1] type = %T, want LiveCommercePaymentPayload", sender.sent[1])
	}
	wantPayment := LiveCommercePaymentPayload{
		EventType:   eventTypeLiveCommercePayment,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		PaymentID:   "pay-1",
		Provider:    "pagarme",
		Method:      "pix",
		Outcome:     "refunded",
		AmountCents: 5000,
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	if payment != wantPayment {
		t.Errorf("payment payload = %+v, want %+v", payment, wantPayment)
	}
}

func TestListeners_OnGMVRecorded(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.GMVRecorded, map[string]any{
		"store_id": "store-1", "cart_id": "cart-1", "amount_cents": 5000, "fee_cents": 250, "billable": true,
	})

	listeners.OnGMVRecorded(t.Context(), env)

	want := LiveCommercePaymentPayload{
		EventType:   eventTypeLiveCommercePayment,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		Outcome:     "recorded",
		AmountCents: 5000,
		FeeCents:    250,
		Billable:    boolPtr(true),
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnGMVRefunded(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.GMVRefunded, map[string]any{
		"store_id": "store-1", "cart_id": "cart-1", "amount_cents": 5000, "fee_credit_cents": 250,
	})

	listeners.OnGMVRefunded(t.Context(), env)

	want := LiveCommercePaymentPayload{
		EventType:   eventTypeLiveCommercePayment,
		Environment: "staging",
		LiveEventID: "live-1",
		StoreID:     "store-1",
		CartID:      "cart-1",
		Outcome:     "refunded",
		AmountCents: 5000,
		FeeCents:    250,
		Timestamp:   env.OccurredAt.UnixMilli(),
	}
	assertSingleSent(t, sender, want)
}

func TestListeners_OnCommentReceived(t *testing.T) {
	t.Parallel()

	commentEnv := func(commentID string) events.Envelope {
		return events.Envelope{
			EventID:     "evt-1",
			Name:        events.CommentReceived,
			Source:      events.Source("instagram"),
			OccurredAt:  time.Unix(1700000000, 0).UTC(),
			Metadata:    map[string]string{"comment_id": commentID},
			LiveEventID: "live-1",
		}
	}

	t.Run("no-ops when no enricher is wired", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)

		listeners.OnCommentReceived(t.Context(), commentEnv("c-1"))

		if len(sender.sent) != 0 {
			t.Errorf("sent = %d events, want 0 without an enricher", len(sender.sent))
		}
	})

	t.Run("no-ops when the envelope carries no comment_id", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)
		enricher, _ := newTestEnricher(&fakeEnrichQueries{})
		listeners.SetEnricher(enricher)

		listeners.OnCommentReceived(t.Context(), events.Envelope{Name: events.CommentReceived})

		if len(sender.sent) != 0 {
			t.Errorf("sent = %d events, want 0 without a comment_id", len(sender.sent))
		}
	})

	t.Run("exports converted_to_cart=true with cart_id/time_to_cart_ms when a cart matched", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)

		row := sqlc.FindCommentCartCorrelationRow{Platform: "instagram", PlatformUserID: "ig-buyer-1"}
		row.EventID = mustUUID(t, "11111111-1111-1111-1111-111111111111")
		row.StoreID = mustUUID(t, "22222222-2222-2222-2222-222222222222")
		row.HasPurchaseIntent.Bool, row.HasPurchaseIntent.Valid = true, true
		row.MatchedProductID = mustUUID(t, "44444444-4444-4444-4444-444444444444")
		row.Result.String, row.Result.Valid = "added_to_cart", true
		commentAt := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
		row.CommentCreatedAt.Time, row.CommentCreatedAt.Valid = commentAt, true
		row.CartID = mustUUID(t, "55555555-5555-5555-5555-555555555555")
		row.ItemCreatedAt.Time, row.ItemCreatedAt.Valid = commentAt.Add(30*time.Second), true

		enricher, _ := newTestEnricher(&fakeEnrichQueries{correlation: row})
		listeners.SetEnricher(enricher)

		env := commentEnv("c-1")
		listeners.OnCommentReceived(t.Context(), env)

		want := LiveCommerceCommentPayload{
			EventType:         eventTypeLiveCommerceComment,
			Environment:       "staging",
			LiveEventID:       "11111111-1111-1111-1111-111111111111",
			StoreID:           "22222222-2222-2222-2222-222222222222",
			Platform:          "instagram",
			PlatformUserID:    "ig-buyer-1",
			HasPurchaseIntent: true,
			MatchedProductID:  "44444444-4444-4444-4444-444444444444",
			Result:            "added_to_cart",
			ConvertedToCart:   true,
			CartID:            "55555555-5555-5555-5555-555555555555",
			TimeToCartMs:      30000,
			Timestamp:         env.OccurredAt.UnixMilli(),
		}
		assertSingleSent(t, sender, want)
	})

	t.Run("exports converted_to_cart=false without cart_id when no cart matched", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)

		row := sqlc.FindCommentCartCorrelationRow{Platform: "instagram", PlatformUserID: "ig-buyer-2"}
		row.EventID = mustUUID(t, "11111111-1111-1111-1111-111111111111")
		row.Result.String, row.Result.Valid = "no_product", true

		enricher, _ := newTestEnricher(&fakeEnrichQueries{correlation: row})
		listeners.SetEnricher(enricher)

		env := commentEnv("c-2")
		listeners.OnCommentReceived(t.Context(), env)

		want := LiveCommerceCommentPayload{
			EventType:       eventTypeLiveCommerceComment,
			Environment:     "staging",
			LiveEventID:     "11111111-1111-1111-1111-111111111111",
			Platform:        "instagram",
			PlatformUserID:  "ig-buyer-2",
			Result:          "no_product",
			ConvertedToCart: false,
			Timestamp:       env.OccurredAt.UnixMilli(),
		}
		assertSingleSent(t, sender, want)
	})

	t.Run("no-ops when the comment isn't persisted yet (query miss)", func(t *testing.T) {
		t.Parallel()

		sender := &fakeSender{}
		listeners, _ := newTestListeners(sender, true)
		enricher, _ := newTestEnricher(&fakeEnrichQueries{correlationErr: errors.New("no rows in result set")})
		listeners.SetEnricher(enricher)

		listeners.OnCommentReceived(t.Context(), commentEnv("c-3"))

		if len(sender.sent) != 0 {
			t.Errorf("sent = %d events, want 0 when the comment row isn't found", len(sender.sent))
		}
	})
}

func TestListeners_Dispatch_routesCommentReceived(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, _ := newTestListeners(sender, true)
	row := sqlc.FindCommentCartCorrelationRow{Platform: "instagram", PlatformUserID: "ig-buyer-1"}
	enricher, _ := newTestEnricher(&fakeEnrichQueries{correlation: row})
	listeners.SetEnricher(enricher)

	listeners.Dispatch(t.Context(), events.Envelope{
		Name:     events.CommentReceived,
		Metadata: map[string]string{"comment_id": "c-1"},
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent = %d events, want 1", len(sender.sent))
	}
	if _, ok := sender.sent[0].(LiveCommerceCommentPayload); !ok {
		t.Fatalf("sent[0] type = %T, want LiveCommerceCommentPayload", sender.sent[0])
	}
}

func TestListeners_Dispatch_routesNewFacts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		event   events.Name
		payload any
		want    any
	}{
		{"cart.item_added", events.CartItemAdded, map[string]any{"cart_id": "cart-1"}, LiveCommerceCartPayload{}},
		{"erp.finalization_failed", events.ERPFinalizationFailed, map[string]any{"cart_id": "cart-1"}, LiveCommerceOpsPayload{}},
		{"notification.failed", events.NotificationFailed, map[string]any{"cart_id": "cart-1"}, LiveCommerceOpsPayload{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sender := &fakeSender{}
			listeners, _ := newTestListeners(sender, true)

			listeners.Dispatch(t.Context(), envelopeWithPayload(t, tc.event, tc.payload))

			if len(sender.sent) != 1 {
				t.Fatalf("sent = %d events, want 1", len(sender.sent))
			}
			gotType := reflect.TypeOf(sender.sent[0])
			wantType := reflect.TypeOf(tc.want)
			if gotType != wantType {
				t.Fatalf("sent[0] type = %v, want %v", gotType, wantType)
			}
		})
	}
}

func TestListeners_decodeFailureIsLoggedAndSwallowed(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	listeners, logs := newTestListeners(sender, true)
	env := events.Envelope{
		EventID: "evt-1",
		Name:    events.EventEventCreated,
		Payload: json.RawMessage(`not-json`),
	}

	listeners.OnEventCreated(t.Context(), env)

	if len(sender.sent) != 0 {
		t.Errorf("sent = %d events, want 0 on decode failure", len(sender.sent))
	}
	if logs.FilterMessageSnippet("decoding payload failed").Len() != 1 {
		t.Errorf("expected exactly one 'decoding payload failed' log entry")
	}
}

func TestListeners_sendFailureIsLoggedAndSwallowed(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{err: errors.New("new relic unavailable")}
	listeners, logs := newTestListeners(sender, true)
	env := envelopeWithPayload(t, events.EventEventCreated, map[string]string{"store_id": "store-1"})

	listeners.OnEventCreated(t.Context(), env)

	if logs.FilterMessageSnippet("sending event failed").Len() != 1 {
		t.Errorf("expected exactly one 'sending event failed' log entry")
	}
}

// assertSingleSent asserts sender received exactly one event equal to want.
func assertSingleSent[T any](t *testing.T, sender *fakeSender, want T) {
	t.Helper()

	if len(sender.sent) != 1 {
		t.Fatalf("sent = %d events, want 1", len(sender.sent))
	}
	got, ok := sender.sent[0].(T)
	if !ok {
		t.Fatalf("sent[0] type = %T, want %T", sender.sent[0], want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %+v, want %+v", got, want)
	}
}

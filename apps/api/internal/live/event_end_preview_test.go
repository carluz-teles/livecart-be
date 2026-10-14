//go:build integration

package live

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
)

func TestEventEndPreviewMatchesFinalization(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct {
		name          string
		closeCart     bool
		eventMinutes  *int
		extended      *int
		storeAutoSend bool
		wantMinutes   int
	}{
		{"event_override", true, endPreviewInt(14400), nil, false, 14400},
		{"five_day_event", true, endPreviewInt(7200), nil, false, 7200},
		{"store_default", true, nil, nil, true, 28800},
		{"extended_store_default", false, endPreviewInt(14400), nil, false, 10080},
		{"extended_event_override", false, endPreviewInt(14400), endPreviewInt(4320), true, 4320},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := seedDeadlineFixture(t, 120, 30)
			if _, err := testPool.Exec(ctx, `UPDATE stores SET cart_expiration_minutes = 28800,
				cart_extended_expiration_minutes = 10080, send_on_live_end = $2 WHERE id = $1`,
				f.storeID, tc.storeAutoSend); err != nil {
				t.Fatal(err)
			}
			// An event's legacy send setting does not override End's empty request.
			if _, err := testPool.Exec(ctx, `UPDATE live_events SET close_cart_on_event_end = $2,
				cart_expiration_minutes = $3, cart_extended_expiration_minutes = $4,
				send_on_live_end = $5 WHERE id = $1`,
				f.eventID, tc.closeCart, tc.eventMinutes, tc.extended, !tc.storeAutoSend); err != nil {
				t.Fatal(err)
			}
			ordinary := f.cart(t, "ordinary", "", false)
			waiting := f.cart(t, "waiting", "waiting", false)
			svc := newWindowService(nil)
			app := fiber.New()
			app.Get("/events/:id", func(c *fiber.Ctx) error {
				c.Locals("store_id", f.storeID)
				return NewHandler(svc, validator.New()).GetByID(c)
			})
			response, err := app.Test(httptest.NewRequest("GET", "/events/"+f.eventID, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("detail status = %d", response.StatusCode)
			}
			var envelope struct {
				Data EventResponse `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			preview := envelope.Data
			if preview.EffectiveCartExpirationMinutes == nil || *preview.EffectiveCartExpirationMinutes != tc.wantMinutes {
				t.Fatalf("effective deadline = %v, want %d", preview.EffectiveCartExpirationMinutes, tc.wantMinutes)
			}
			if preview.AutoSendCheckoutLinks == nil || *preview.AutoSendCheckoutLinks != tc.storeAutoSend {
				t.Fatalf("auto send = %v, want %v (including explicit false in JSON)", preview.AutoSendCheckoutLinks, tc.storeAutoSend)
			}
			if preview.Status != "active" || readDeadline(t, ordinary) != nil {
				t.Fatal("reading the preview must not close the event or start cart deadlines")
			}

			ended, err := svc.End(ctx, EndLiveInput{ID: f.eventID, StoreID: f.storeID})
			if err != nil {
				t.Fatal(err)
			}
			if ended.AutoSendLinks != *preview.AutoSendCheckoutLinks {
				t.Fatal("preview differs from finalization's notification setting")
			}
			var closedAt time.Time
			if err := testPool.QueryRow(ctx, `SELECT commercial_closed_at FROM live_events WHERE id = $1`, f.eventID).Scan(&closedAt); err != nil {
				t.Fatal(err)
			}
			assertDeadline(t, ordinary, closedAt.Add(time.Duration(tc.wantMinutes)*time.Minute))
			assertDeadline(t, waiting, closedAt.Add(time.Duration(tc.wantMinutes+30)*time.Minute))
		})
	}
}

func TestEventEndPreviewDoesNotExposeOtherStore(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedDeadlineFixture(t, 14400, 30)
	otherStore := seedWindowStore(t, ctx, "end-preview-other-store")
	output, err := newWindowService(nil).GetEventWithSessions(ctx, f.eventID, otherStore)
	if err == nil || output.EffectiveCartExpirationMinutes != nil || output.AutoSendCheckoutLinks != nil {
		t.Fatal("another store must not receive the event or its resolved closing settings")
	}
}

func endPreviewInt(value int) *int { return &value }

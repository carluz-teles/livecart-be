package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestResolveLiveEventID proves the /events/:id and /lives/:id (plus the
// coupon /events/:eventId/coupons alias) path segment is read as the
// live_event_id, and that unrelated routes — including the static
// sub-routes registered right after "/events"/"/lives" in
// internal/live/handler.go (List/Create at "/", "/stats", "/posts"), and any
// future static sub-route that isn't a UUID — never produce a false
// positive.
func TestResolveLiveEventID(t *testing.T) {
	t.Parallel()

	const liveEventID = "33333333-3333-3333-3333-333333333333"

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "events/:id root",
			path: "/api/v1/stores/store-1/events/" + liveEventID,
			want: liveEventID,
		},
		{
			name: "events/:id/start nested action",
			path: "/api/v1/stores/store-1/events/" + liveEventID + "/start",
			want: liveEventID,
		},
		{
			name: "lives/:id alias",
			path: "/api/v1/stores/store-1/lives/" + liveEventID + "/end",
			want: liveEventID,
		},
		{
			name: "events/:eventId/coupons alias",
			path: "/api/v1/stores/store-1/events/" + liveEventID + "/coupons",
			want: liveEventID,
		},
		{
			name: "events list (no id segment)",
			path: "/api/v1/stores/store-1/events",
			want: "",
		},
		{
			name: "events/stats static sub-route",
			path: "/api/v1/stores/store-1/events/stats",
			want: "",
		},
		{
			name: "events/posts static sub-route",
			path: "/api/v1/stores/store-1/events/posts",
			want: "",
		},
		{
			name: "unrelated route with no events/lives segment",
			path: "/api/v1/stores/store-1/dashboard/analytics/events",
			want: "",
		},
		{
			name: "events/export hypothetical future static sub-route (non-UUID segment)",
			path: "/api/v1/stores/store-1/events/export",
			want: "",
		},
		{
			name: "lives/search hypothetical future static sub-route (non-UUID segment)",
			path: "/api/v1/stores/store-1/lives/search",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got string
			app := fiber.New()
			app.Use(func(c *fiber.Ctx) error {
				got = resolveLiveEventID(c)
				return c.Next()
			})
			app.All("/*", func(c *fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if got != tt.want {
				t.Fatalf("resolveLiveEventID(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

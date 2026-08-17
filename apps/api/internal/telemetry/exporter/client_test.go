package exporter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

func TestNRClient_SendEvent(t *testing.T) {
	t.Run("posts the event as a one-item batch with the Api-Key header", func(t *testing.T) {
		t.Parallel()

		var gotAPIKey string
		var gotBody []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAPIKey = r.Header.Get("Api-Key")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		client := newTestNRClient(t, srv.URL, "license-key")

		event := NewLiveCommerceEventPayload()
		event.LiveEventID = "live-1"
		event.Action = "created"

		if err := client.SendEvent(t.Context(), event); err != nil {
			t.Fatalf("SendEvent() error = %v, want nil", err)
		}

		if gotAPIKey != "license-key" {
			t.Errorf("Api-Key header = %q, want %q", gotAPIKey, "license-key")
		}
		if len(gotBody) != 1 {
			t.Fatalf("request body item count = %d, want 1", len(gotBody))
		}
		if gotBody[0]["eventType"] != eventTypeLiveCommerceEvent {
			t.Errorf("eventType = %v, want %v", gotBody[0]["eventType"], eventTypeLiveCommerceEvent)
		}
		if gotBody[0]["live_event_id"] != "live-1" {
			t.Errorf("live_event_id = %v, want live-1", gotBody[0]["live_event_id"])
		}
	})

	t.Run("returns an error on a 4xx response without retrying", func(t *testing.T) {
		t.Parallel()

		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"invalid license key"}`))
		}))
		defer srv.Close()

		client := newTestNRClient(t, srv.URL, "bad-key")

		err := client.SendEvent(t.Context(), NewLiveCommerceEventPayload())
		if err == nil {
			t.Fatal("SendEvent() error = nil, want non-nil")
		}
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Errorf("attempts = %d, want 1 (4xx must not retry)", got)
		}
	})

	t.Run("retries on 5xx and eventually returns an error when it never recovers", func(t *testing.T) {
		t.Parallel()

		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := newTestNRClient(t, srv.URL, "license-key")

		err := client.SendEvent(t.Context(), NewLiveCommerceEventPayload())
		if err == nil {
			t.Fatal("SendEvent() error = nil, want non-nil")
		}
		// maxRetries=2 means 3 total attempts (initial + 2 retries).
		if got := atomic.LoadInt32(&attempts); got != int32(maxRetries+1) {
			t.Errorf("attempts = %d, want %d", got, maxRetries+1)
		}
	})

	t.Run("retries on 5xx and succeeds once the server recovers", func(t *testing.T) {
		t.Parallel()

		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		client := newTestNRClient(t, srv.URL, "license-key")

		if err := client.SendEvent(t.Context(), NewLiveCommerceEventPayload()); err != nil {
			t.Fatalf("SendEvent() error = %v, want nil", err)
		}
		if got := atomic.LoadInt32(&attempts); got != 2 {
			t.Errorf("attempts = %d, want 2", got)
		}
	})
}

// newTestNRClient builds an NRClient pointed at a local httptest server
// instead of the real New Relic endpoint.
func newTestNRClient(t *testing.T, url, apiKey string) *NRClient {
	t.Helper()

	client := NewNRClient(Config{APIKey: apiKey, AccountID: "8291202", Enabled: true}, zap.NewNop())
	client.url = url

	return client
}

package social

import (
	"errors"
	"net/http"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestPrivateReplyClassifiesBusinessRefusalUnderHTTP500(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		retryable bool
		calls     int
	}{
		{"already replied", 500, `{"error":{"code":-1,"error_subcode":2534023}}`, false, 1},
		{"window closed", 500, `{"error":{"code":10,"error_subcode":2534022}}`, false, 1},
		{"forbidden", 403, `{"error":{"code":10}}`, false, 1},
		{"temporary server error", 500, `{"error":{"message":"unknown"}}`, true, 3},
		{"rate limit", 429, `{}`, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ig, cleanup := newTestInstagram(t, &providers.Credentials{AccessToken: "test"}, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			defer cleanup()
			err := ig.SendPrivateReply(t.Context(), "comment", "message")
			var delivery *providers.DeliveryError
			if !errors.As(err, &delivery) || delivery.Retryable != tc.retryable || calls != tc.calls {
				t.Fatalf("calls=%d error=%v; want calls=%d retryable=%v", calls, err, tc.calls, tc.retryable)
			}
		})
	}
}

package live

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestRetryableNotificationErrorIncludesFallback(t *testing.T) {
	permanent := &providers.DeliveryError{Err: errors.New("reply already used"), Retryable: false}
	temporary := &providers.DeliveryError{Err: errors.New("DM rate limited"), Retryable: true}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"delivered", nil, false},
		{"both paths closed", errors.Join(permanent, permanent), false},
		{"fallback temporary", errors.Join(permanent, temporary), true},
		{"reply temporary", errors.Join(temporary, permanent), true},
		{"wrapped fallback", fmt.Errorf("delivery: %w", errors.Join(permanent, temporary)), true},
		{"cancelled fallback", errors.Join(permanent, context.DeadlineExceeded), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableNotificationError(tc.err); got != tc.want {
				t.Fatalf("retry=%v want=%v", got, tc.want)
			}
		})
	}
}

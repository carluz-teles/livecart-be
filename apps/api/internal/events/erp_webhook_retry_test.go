package events

import (
	"testing"
	"time"
)

func TestERPWebhookRetryWindowSurvivesDailyQuotaButExpires(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    Name
		age     time.Duration
		expired bool
	}{
		{ERPWebhookProcess, 25 * time.Hour, false},
		{ERPWebhookProcess, 71 * time.Hour, false},
		{ERPWebhookProcess, 72 * time.Hour, true},
		{ERPWebhookProcess, 96 * time.Hour, true},
		{OrderPaid, 96 * time.Hour, false},
	} {
		env := Envelope{Name: tc.name, OccurredAt: now.Add(-tc.age)}
		if expired := env.ERPWebhookExpired(now); expired != tc.expired {
			t.Errorf("%s after %s expired=%v, want %v", tc.name, tc.age, expired, tc.expired)
		}
	}
	if !(Envelope{Name: ERPWebhookProcess}).ERPWebhookExpired(now) {
		t.Fatal("webhook without a receipt timestamp must not bypass retry window")
	}
}

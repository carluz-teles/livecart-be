package notification

import "testing"

func TestTestRecipientConfigured(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recipient TestRecipient
		want      bool
	}{
		{"webhook sender without public handle", TestRecipient{PSID: "sender"}, true},
		{"sender with handle", TestRecipient{PSID: "sender", Handle: "buyer"}, true},
		{"handle alone cannot receive a DM", TestRecipient{Handle: "buyer"}, false},
		{"not configured", TestRecipient{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.recipient.Configured(); got != tc.want {
				t.Fatalf("configured=%v want=%v", got, tc.want)
			}
		})
	}
}

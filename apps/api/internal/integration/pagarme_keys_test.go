package integration

import "testing"

// Regression guard for the production outage where the connect form rejected
// every real Pagar.me production key: the code assumed sk_live_ / pk_live_
// (Stripe's convention). Pagar.me only tags SANDBOX keys with test_; production
// keys are plain sk_ / pk_ + token.
func TestPagarmeKeyEnvironment(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		scope string
		want  string
	}{
		{"sandbox secret", "sk_test_abc123", "sk_", "sandbox"},
		{"sandbox public", "pk_test_abc123", "pk_", "sandbox"},
		// The case that broke production: a live key carries no env segment.
		{"production secret", "sk_abc123def456", "sk_", "production"},
		{"production public", "pk_abc123def456", "pk_", "production"},
		// Stripe-style keys must not be mistaken for sandbox.
		{"stripe-style live secret is still production", "sk_live_abc", "sk_", "production"},

		{"scope prefix only, no token", "sk_", "sk_", ""},
		{"public key given where secret expected", "pk_abc123", "sk_", ""},
		{"secret key given where public expected", "sk_abc123", "pk_", ""},
		{"empty", "", "sk_", ""},
		{"garbage", "not-a-key", "sk_", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pagarmeKeyEnvironment(tc.key, tc.scope); got != tc.want {
				t.Fatalf("pagarmeKeyEnvironment(%q, %q) = %q, want %q",
					tc.key, tc.scope, got, tc.want)
			}
		})
	}
}

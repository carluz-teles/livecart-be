package billing

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// hasFieldErr reports whether err is a validation.Errors carrying the given
// json field key. Returns false if err is nil or not a validation.Errors.
func hasFieldErr(err error, field string) bool {
	verrs, ok := err.(validation.Errors)
	if !ok {
		return false
	}
	_, present := verrs[field]
	return present
}

func TestCreateCheckoutRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		req      CreateCheckoutRequest
		wantErr  bool
		errField string // json key expected in validation.Errors (optional)
	}{
		// exactly one valid case per interval
		{"valid monthly", CreateCheckoutRequest{Interval: "monthly"}, false, ""},
		{"valid semestral", CreateCheckoutRequest{Interval: "semestral"}, false, ""},
		{"valid annual", CreateCheckoutRequest{Interval: "annual"}, false, ""},
		// Required rule
		{"missing interval (Required)", CreateCheckoutRequest{Interval: ""}, true, "interval"},
		// In rule — plan names or garbage are not valid intervals
		{"interval not in set (plan name)", CreateCheckoutRequest{Interval: "pro"}, true, "interval"},
		{"interval not in set (garbage)", CreateCheckoutRequest{Interval: "nope"}, true, "interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.errField != "" && !hasFieldErr(err, tc.errField) {
				t.Fatalf("expected error on field %q, got %v", tc.errField, err)
			}
		})
	}
}

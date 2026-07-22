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
		// exactly one valid case
		{"valid start", CreateCheckoutRequest{Plan: "start"}, false, ""},
		// Required rule
		{"missing plan (Required)", CreateCheckoutRequest{Plan: ""}, true, "plan"},
		// In rule — enterprise is not self-service / not contractable
		{"plan not in set (enterprise)", CreateCheckoutRequest{Plan: "enterprise"}, true, "plan"},
		{"plan not in set (garbage)", CreateCheckoutRequest{Plan: "nope"}, true, "plan"},
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

func TestChangePlanRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		req      ChangePlanRequest
		wantErr  bool
		errField string
	}{
		// exactly one valid case
		{"valid grow", ChangePlanRequest{Plan: "grow"}, false, ""},
		// Required rule
		{"missing plan (Required)", ChangePlanRequest{Plan: ""}, true, "plan"},
		// In rule
		{"plan not in set (enterprise)", ChangePlanRequest{Plan: "enterprise"}, true, "plan"},
		{"plan not in set (garbage)", ChangePlanRequest{Plan: "xxx"}, true, "plan"},
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

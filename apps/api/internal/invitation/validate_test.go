package invitation

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// hasFieldErr reports whether err is a validation.Errors carrying the given json key.
func hasFieldErr(err error, key string) bool {
	verrs, ok := err.(validation.Errors)
	if !ok {
		return false
	}
	_, present := verrs[key]
	return present
}

func TestCreateInvitationRequestValidate(t *testing.T) {
	valid := CreateInvitationRequest{
		Email: "user@example.com",
		Role:  "member",
	}

	cases := []struct {
		name    string
		req     CreateInvitationRequest
		wantErr bool
		field   string
	}{
		{"valid member", valid, false, ""},
		{"valid admin", CreateInvitationRequest{Email: "admin@example.com", Role: "admin"}, false, ""},

		// Email
		{"email missing", CreateInvitationRequest{Role: "member"}, true, "email"},
		{"email invalid format", CreateInvitationRequest{Email: "not-an-email", Role: "member"}, true, "email"},

		// Role
		{"role missing", CreateInvitationRequest{Email: "user@example.com"}, true, "role"},
		{"role not in set", CreateInvitationRequest{Email: "user@example.com", Role: "owner"}, true, "role"},
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
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}

func TestAcceptInvitationRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     AcceptInvitationRequest
		wantErr bool
		field   string
	}{
		{"valid", AcceptInvitationRequest{Token: "some-token"}, false, ""},

		// Token
		{"token missing", AcceptInvitationRequest{}, true, "token"},
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
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}

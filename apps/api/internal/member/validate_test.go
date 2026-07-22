package member

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// hasFieldKey reports whether err is a validation.Errors map that contains
// the given json field key.
func hasFieldKey(err error, key string) bool {
	verrs, ok := err.(validation.Errors)
	if !ok {
		return false
	}
	_, present := verrs[key]
	return present
}

func TestUpdateMemberRoleRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		req      UpdateMemberRoleRequest
		wantErr  bool
		fieldKey string
	}{
		{
			name:    "valid: admin",
			req:     UpdateMemberRoleRequest{Role: "admin"},
			wantErr: false,
		},
		{
			name:    "valid: member",
			req:     UpdateMemberRoleRequest{Role: "member"},
			wantErr: false,
		},
		{
			name:     "invalid: role required (empty)",
			req:      UpdateMemberRoleRequest{Role: ""},
			wantErr:  true,
			fieldKey: "role",
		},
		{
			name:     "invalid: role not in enum (owner not settable)",
			req:      UpdateMemberRoleRequest{Role: "owner"},
			wantErr:  true,
			fieldKey: "role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.fieldKey != "" && !hasFieldKey(err, tc.fieldKey) {
					t.Fatalf("expected error to contain field key %q, got %v", tc.fieldKey, err)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

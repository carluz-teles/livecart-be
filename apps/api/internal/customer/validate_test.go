package customer

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

func TestBlockHandleRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		req      BlockHandleRequest
		wantErr  bool
		fieldKey string
	}{
		{
			name:    "valid: handle set, reason optional present",
			req:     BlockHandleRequest{Handle: "joana", Reason: "spam"},
			wantErr: false,
		},
		{
			name:    "valid: handle set, reason omitted",
			req:     BlockHandleRequest{Handle: "joana"},
			wantErr: false,
		},
		{
			name:     "invalid: handle required (empty)",
			req:      BlockHandleRequest{Handle: ""},
			wantErr:  true,
			fieldKey: "handle",
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

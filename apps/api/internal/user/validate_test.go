package user

import "testing"

// SyncUserRequest has no client-supplied fields; Validate() is a no-op that
// always passes (satisfies the httpx.Validatable contract). There are no
// validation rules to violate, so only the valid case is meaningful.
func TestSyncUserRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     SyncUserRequest
		wantErr bool
	}{
		{"valid empty body", SyncUserRequest{}, false},
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
		})
	}
}

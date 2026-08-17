package order

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// strPtr is a small helper to build *string fixtures for the optional
// pointer fields on UpdateOrderRequest.
func strPtr(s string) *string { return &s }

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

func TestUpdateOrderRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		req      UpdateOrderRequest
		wantErr  bool
		fieldKey string // json key expected in the error, "" to skip the assertion
	}{
		{
			name:    "valid: both enums allowed",
			req:     UpdateOrderRequest{Status: strPtr("active"), PaymentStatus: strPtr("paid")},
			wantErr: false,
		},
		{
			name:    "valid: both nil (fields optional)",
			req:     UpdateOrderRequest{},
			wantErr: false,
		},
		{
			name:     "invalid: status not in enum",
			req:      UpdateOrderRequest{Status: strPtr("bogus")},
			wantErr:  true,
			fieldKey: "status",
		},
		{
			name:     "invalid: paymentStatus not in enum",
			req:      UpdateOrderRequest{PaymentStatus: strPtr("bogus")},
			wantErr:  true,
			fieldKey: "paymentStatus",
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

// validAddress returns a fully-populated valid UpdateShippingAddressRequest
// that each invalid sub-case mutates.
func validAddress() UpdateShippingAddressRequest {
	return UpdateShippingAddressRequest{
		ZipCode:      "01310100",
		Street:       "Av Paulista",
		Number:       "1000",
		Complement:   "Apto 12",
		Neighborhood: "Bela Vista",
		City:         "Sao Paulo",
		State:        "SP",
	}
}

func TestUpdateShippingAddressRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*UpdateShippingAddressRequest)
		wantErr  bool
		fieldKey string
	}{
		{
			name:    "valid: fully populated",
			mutate:  func(r *UpdateShippingAddressRequest) {},
			wantErr: false,
		},
		{
			name:     "invalid: zipCode required",
			mutate:   func(r *UpdateShippingAddressRequest) { r.ZipCode = "" },
			wantErr:  true,
			fieldKey: "zipCode",
		},
		{
			name:     "invalid: street required",
			mutate:   func(r *UpdateShippingAddressRequest) { r.Street = "" },
			wantErr:  true,
			fieldKey: "street",
		},
		{
			name:     "invalid: number required",
			mutate:   func(r *UpdateShippingAddressRequest) { r.Number = "" },
			wantErr:  true,
			fieldKey: "number",
		},
		{
			name:     "invalid: neighborhood required",
			mutate:   func(r *UpdateShippingAddressRequest) { r.Neighborhood = "" },
			wantErr:  true,
			fieldKey: "neighborhood",
		},
		{
			name:     "invalid: city required",
			mutate:   func(r *UpdateShippingAddressRequest) { r.City = "" },
			wantErr:  true,
			fieldKey: "city",
		},
		{
			name:     "invalid: state required",
			mutate:   func(r *UpdateShippingAddressRequest) { r.State = "" },
			wantErr:  true,
			fieldKey: "state",
		},
		{
			name:     "invalid: state must be 2 chars (too long)",
			mutate:   func(r *UpdateShippingAddressRequest) { r.State = "SPX" },
			wantErr:  true,
			fieldKey: "state",
		},
		{
			name:     "invalid: state must be 2 chars (too short)",
			mutate:   func(r *UpdateShippingAddressRequest) { r.State = "S" },
			wantErr:  true,
			fieldKey: "state",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validAddress()
			tc.mutate(&req)
			err := req.Validate()
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
